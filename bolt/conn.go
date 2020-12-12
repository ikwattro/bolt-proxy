package bolt

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"

	"github.com/gobwas/ws"
)

// An abstraction of a Bolt-aware io.ReadWriterCloser. Allows for sending and
// receiving Messages, abstracting away the nuances of the transport.
type BoltConn interface {
	R() <-chan *Message
	WriteMessage(*Message) error
	io.Closer
}

// Designed for operating direct (e.g. TCP/IP-only) Bolt connections
type DirectConn struct {
	conn     io.ReadWriteCloser
	buf      []byte
	r        <-chan *Message
	chunking bool
}

// Used for WebSocket-based Bolt connections
type WsConn struct {
	conn     io.ReadWriteCloser
	buf      []byte
	r        <-chan *Message
	chunking bool
}

func NewDirectConn(c io.ReadWriteCloser) DirectConn {
	msgchan := make(chan *Message)
	dc := DirectConn{
		conn:     c,
		buf:      make([]byte, 1024*128),
		r:        msgchan,
		chunking: false,
	}

	for i := 0; i < len(dc.buf); i++ {
		dc.buf[i] = 0xff
	}

	go func() {
		for {
			message, err := dc.readMessage()
			if err != nil {
				if err == io.EOF {
					log.Println("direct bolt connection hung-up")
					close(msgchan)
					return
				}
				log.Printf("direct bolt connection error! %s\n", err)
				return
			}
			msgchan <- message
		}
	}()

	return dc
}

func (c DirectConn) R() <-chan *Message {
	return c.r
}

// Read a single bolt Message, returning a point to it, or an error
func (c DirectConn) readMessage() (*Message, error) {
	var n int
	var err error

	underReads := 0
	pos := 0
	for {
		n, err = c.conn.Read(c.buf[pos : pos+2])
		if err != nil {
			return nil, err
		}
		// TODO: deal with this horrible issue!
		if n < 2 {
			underReads++
			if underReads > 5 {
				panic("too many under reads")
			}
			continue
			//panic("under-read?!")
		}
		msglen := int(binary.BigEndian.Uint16(c.buf[pos : pos+n]))
		pos = pos + n

		if msglen < 1 {
			// 0x00 0x00 would mean we're done
			break
		}

		endOfData := pos + msglen
		// handle short reads of user data
		for pos < endOfData {
			n, err = c.conn.Read(c.buf[pos:endOfData])
			if err != nil {
				return nil, err
			}
			pos = pos + n
		}
	}

	t := IdentifyType(c.buf[:pos])

	// Copy data into Message...
	data := make([]byte, pos)
	copy(data, c.buf[:pos])

	for i := 0; i < pos; i++ {
		c.buf[i] = 0xff
	}

	return &Message{
		T:    t,
		Data: data,
	}, nil
}

func (c DirectConn) WriteMessage(m *Message) error {
	// TODO validate message?

	n, err := c.conn.Write(m.Data)
	if err != nil {
		return err
	}
	if n != len(m.Data) {
		// TODO: loop to write all data?
		panic("incomplete message written")
	}

	return nil
}

func (c DirectConn) Close() error {
	return c.conn.Close()
}

func NewWsConn(c io.ReadWriteCloser) WsConn {
	msgchan := make(chan *Message)
	ws := WsConn{
		conn:     c,
		buf:      make([]byte, 1024*32),
		r:        msgchan,
		chunking: false,
	}

	// 0xff out the buffer
	for i := 0; i < len(ws.buf); i++ {
		ws.buf[i] = 0xff
	}

	go func() {
		for {
			messages, err := ws.readMessages()
			if err != nil {
				if err == io.EOF {
					log.Println("bolt ws connection hung-up")
					close(msgchan)
					return
				}
				log.Printf("ws bolt connection error! %s\n", err)
				return
			}
			for _, message := range messages {
				if message == nil {
					panic("ws message = nil!")
				}
				msgchan <- message
			}
		}
	}()

	return ws
}

func (c WsConn) R() <-chan *Message {
	return c.r
}

// Read 0 or many Bolt Messages from a WebSocket frame since, apparently,
// small Bolt Messages sometimes get packed into a single Frame(?!).
//
// For example, I've seen RUN + PULL all in 1 WebSocket frame.
func (c WsConn) readMessages() ([]*Message, error) {
	messages := make([]*Message, 0)

	header, err := ws.ReadHeader(c.conn)
	if err != nil {
		return nil, err
	}

	if !header.Fin {
		panic("unsupported header fin")
	}

	switch header.OpCode {
	case ws.OpClose:
		return nil, io.EOF
	case ws.OpPing, ws.OpPong, ws.OpContinuation, ws.OpText:
		panic(fmt.Sprintf("unsupported websocket opcode: %v\n", header.OpCode))
		// return nil, errors.New(msg)
	}

	// TODO: handle header.Length == 0 situations?
	if header.Length == 0 {
		return nil, errors.New("zero length header?!")
	}

	// TODO: under-reads!!!
	n, err := c.conn.Read(c.buf[:header.Length])
	if err != nil {
		return nil, err
	}

	if header.Masked {
		ws.Cipher(c.buf[:n], header.Mask, 0)
		header.Masked = false
	}

	// WebSocket frames might contain multiple bolt messages...oh, joy
	// XXX: for now we don't look for chunks across frame boundaries
	pos := 0

	for pos < int(header.Length) {
		msglen := int(binary.BigEndian.Uint16(c.buf[pos : pos+2]))

		// since we've already got the data in our buffer, we can
		// peek to see if we're about to or still chunking (or not)
		if bytes.Equal([]byte{0x0, 0x0}, c.buf[pos+msglen+2:pos+msglen+4]) {
			c.chunking = false
		} else {
			c.chunking = true
		}

		// we'll let the combination of the type and the chunking
		// flag dictate behavior as we're not cleaning our buffer
		// afterwards, so maaaaaybe there was a false positive
		sizeOfMsg := msglen + 4
		msgtype := IdentifyType(c.buf[pos:])
		if msgtype == UnknownMsg {
			msgtype = ChunkedMsg
		}
		if c.chunking {
			sizeOfMsg = msglen + 2
		}

		data := make([]byte, sizeOfMsg)
		copy(data, c.buf[pos:pos+sizeOfMsg])
		msg := Message{
			T:    msgtype,
			Data: data,
		}
		//fmt.Printf("**** appending msg: %#v\n", msg)
		messages = append(messages, &msg)

		pos = pos + sizeOfMsg
	}

	// we need to 0xff out the buffer to prevent any secrets residing
	// in memory, but also so we don't get false 0x00 0x00 padding
	for i := 0; i < n; i++ {
		c.buf[i] = 0xff
	}

	fmt.Printf("**** parsed %d ws bolt messages\n", len(messages))

	return messages, nil
}
func (c WsConn) WriteMessage(m *Message) error {
	frame := ws.NewBinaryFrame(m.Data)
	err := ws.WriteFrame(c.conn, frame)

	return err
}

func (c WsConn) Close() error {
	return c.conn.Close()
}
