package siphon

import (
	"encoding/json"
	"fmt"
	"net"
	"sync"
)

func NewNetConn(netconn net.Conn) *Conn {
	return &Conn{
		label: fmt.Sprintf("%p", netconn),
		decoder: json.NewDecoder(netconn),
		encoder: json.NewEncoder(netconn),
		closerFn: func() error {
			return netconn.Close()
		},
	}
}

type Conn struct {
	label       string
	decoder     *json.Decoder
	decoderLock sync.Mutex
	encoder     *json.Encoder
	encoderLock sync.Mutex
	closerFn    func()error
}

func (conn *Conn) Label() string {
	return conn.label
}

func (conn *Conn) Decode(v interface{}) error {
	conn.decoderLock.Lock()
	defer conn.decoderLock.Unlock()
	return conn.decoder.Decode(v)
}

func (conn *Conn) Encode(v interface{}) error {
	conn.encoderLock.Lock()
	defer conn.encoderLock.Unlock()
	return conn.encoder.Encode(v)
}

func (conn *Conn) Close() error {
	return conn.closerFn()
}

type Message struct {
	Content     []byte    `json:",omitempty"`
	TtyHeight   int       `json:",omitempty"`
	TtyWidth    int       `json:",omitempty"`

	// Future: should we be detecting ansi escape codes and buffering that kind of state for new clients?
	//   So i.e. color codes in effect mid-client-attach give the client the right color,
	//   and attaching to vim starts your cursor in the right place?
}

/**
 * First message sent after dialing a connection.
 */
type Hello struct {
	Siphon string
	/** Only the value "client" is currently expected. */
	Hello string
}

type HelloAck struct {
	Siphon string
	/**
	 * Values may be "server" or "daemon", which the client will use to
	 * determine what kind of messages it expects to come next.
	 */
	Hello string
}

/**
 * Message passed from daemon to client telling the client where its new host is.
 * This message immediately follows a HelloAck that annouces the daemon as a daemon,
 * and the connection can be expected to shutdown after this message.
 */
type Redirect struct {
	Addr Addr
}
