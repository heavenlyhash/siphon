package siphon

import (
	"github.com/dotcloud/docker/term"
	"github.com/kr/pty"
	"io"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sync"
	"syscall"
)

func NewHost(cmd *exec.Cmd, siphon Addr) (host Host) {
	host = Host{}
	host.siphon = siphon
	host.cmd = cmd
	host.stdout = NewWriteBroadcaster()
	host.stdin, host.stdinPipe = io.Pipe()
	host.exitCh = make(chan bool)
	return
}

type Host struct {
	siphon    Addr
	cmd       *exec.Cmd
	stdout    *WriteBroadcaster
	stdin     io.ReadCloser
	stdinPipe io.WriteCloser
	pty       *os.File
	exitCh    chan bool
	exitCode  int

	listener  net.Listener
}

func (host *Host) Serve() error {
	if host.siphon.Proto == "internal" {
		return nil
	}
	fmt.Fprintf(log.host, "preparing to accept client connections\r\n")
	listener, err := net.Listen(host.siphon.Proto, host.siphon.Addr)
	if err != nil {
		return err
	}
	host.listener = listener
	go func() {
		for host.listener != nil {
			conn, err := host.listener.Accept();
			// if err.Err == net.errClosing {
			// 	break
			// } // I can't do this because net.errClosing isn't visible to me.  Yay for go's whimsically probably-weakly typed errors.
			if err != nil {
				// Also I can't check if host.listener is closed because there's no such predicate.  Good good.  Not that that wouldn't be asking for a race condition anyway.
				if err.(*net.OpError).Err.Error() == "use of closed network connection" {
					// doing this strcmp makes me feel absolutely awful, but a closed socket is normal shutdown and not panicworthy in the slightest, and I can't for the life of me find any saner way to distinguish that.
					break
				}
				panic(err)
			}
			fmt.Fprintf(log.host, "accepted new client connection %p\r\n", conn)
			go host.handleRemoteClient(NewNetConn(conn))
		}
	}()
	return nil
}

func (host *Host) handleRemoteClient(conn *Conn) {
	defer conn.Close()
	var track sync.WaitGroup

	// do startup handshake
	var hai Hello
	if err := conn.Decode(&hai); err != nil {
		fmt.Fprintf(log.host, "%s, dropping client %s", err, conn.Label())
		return
	}
	if hai.Siphon != "siphon" {
		fmt.Fprintf(log.host, "Encountered a non-siphon.Protocol, dropping client %s", conn.Label())
		return
	}
	if hai.Hello != "client" {
		fmt.Fprintf(log.host, "Unexpected hello from not a client protocol, dropping client %s", conn.Label())
		return
	}
	if err := conn.Encode(HelloAck{
		Siphon: "siphon",
		Hello: "server",
	}); err != nil {
		fmt.Fprintf(log.host, "%s, dropping client %s", err, conn.Label())
		return
	}

	// recieve client input and resize requests
	track.Add(1)
	go func() {
		in := host.StdinPipe()
		for {
			var m Message
			if err := conn.Decode(&m); err != nil {	//FIXME: this will happily hang out long after cmd has exited if the client fails to close.
				break
			}
			if m.Content != nil {
				if _, err := in.Write(m.Content); err != nil {
					panic(err)
				}
			} else if m.TtyHeight != 0 && m.TtyWidth != 0 {
				host.Resize(m.TtyHeight, m.TtyWidth)
				//TODO: conn.Write(json.Marshal(Message{TtyHeight:m.TtyHeight, ...}))
			}
		}
		fmt.Fprintf(log.host, "client %s closed client input\r\n", conn.Label())
		track.Done()
	}()

	// send pty output and size changes
	track.Add(1)
	go func() {
		out := host.StdoutPipe()
		buf := make([]byte, 32*1024)
		for {
			nr, err := out.Read(buf)
			if nr > 0 {
				m := Message{Content:buf[0:nr]}
				if err := conn.Encode(&m); err != nil {
					break
				}
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				panic(err)
			}
		}
		fmt.Fprintf(log.host, "client %s output closed\r\n", conn.Label())
		conn.Close()
		track.Done()
	}()

	track.Wait()
}

func (host *Host) UnServe() {
	switch x := host.listener.(type) {
	case nil:
		return
	default:
		fmt.Fprintf(log.host, "halting accept of new client connections\r\n")
		x.Close()
		host.listener = nil
	}
}

func (host *Host) Start() {
	pty, ptySlave, err := pty.Open()
	if err != nil {
		panic(err)
	}
	host.pty = pty
	host.cmd.Stdout = ptySlave
	host.cmd.Stderr = ptySlave

	// copy output from the pty to our broadcasters
	go func() {
		defer host.stdout.CloseWriters()
		io.Copy(host.stdout, pty)
	}()

	// copy stdin from our pipe to the pty
	host.cmd.Stdin = ptySlave
	host.cmd.SysProcAttr = &syscall.SysProcAttr{Setctty: true, Setsid: true}
	go func() {
		defer host.stdin.Close()
		io.Copy(pty, host.stdin)
	}()

	// rets roll
	fmt.Fprintf(log.host, "launching hosted process...\r\n")
	if err := host.cmd.Start(); err != nil {
		panic(err)
	}
	go func() {
		err := host.cmd.Wait()
		if exitError, ok := err.(*exec.ExitError); ok {
			if waitStatus, ok := exitError.Sys().(syscall.WaitStatus); ok {
				host.exitCode = waitStatus.ExitStatus()
			} else { panic(exitError); }
		}// else { panic(err); }
		close(host.exitCh)
	}()
	go func() {
		exitCode := host.Wait()
		fmt.Fprintf(log.host, "hosted process exited %d\r\n", exitCode)
	}()
	ptySlave.Close()
}

func (host *Host) Wait() int {
	<- host.exitCh
	return host.exitCode
}

func (host *Host) StdinPipe() io.WriteCloser {
	return host.stdinPipe
}

func (host *Host) StdoutPipe() io.ReadCloser {
	reader, writer := io.Pipe()
	host.stdout.AddWriter(writer)
	return reader
	// DELTA: docker wraps the reader in a NewBufReader before returning.  not sure i find this the right layer for that.
}

func (host *Host) Resize(h int, w int) {
	fmt.Fprintf(log.host, "resizing pty to h=%d w=%d\r\n", h, w)
	err := term.SetWinsize(host.pty.Fd(), &term.Winsize{Height: uint16(h), Width: uint16(w)})
	if err.(syscall.Errno) != 0 {
		panic(fmt.Errorf("siphon: host error setting terminal size: %s\n", err))
	}
}

func (host *Host) cleanup() {
	if err := host.stdin.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "siphon: cleanup on %s host failed to close stdin: %s\n", host.siphon.Label, err)
	}
	host.stdout.CloseWriters()
	if err := host.pty.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "siphon: cleanup on %s host failed to close pty: %s\n", host.siphon.Label, err)
	}
}


