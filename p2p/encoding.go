package p2p

import (
	"encoding/gob"
	"fmt"
	"io"
)

type Decoder interface {
	Decode(io.Reader, *RPC) error
}

type GOBDecoder struct{}

func (dec GOBDecoder) Decode(r io.Reader, msg *RPC) error {
	return gob.NewDecoder(r).Decode(msg)
}

type DefaultDecoder struct{}

// In encoding.go - Update the DefaultDecoder
func (dec DefaultDecoder) Decode(r io.Reader, msg *RPC) error {
	// Read the first byte to check for stream flag
	peekBuf := make([]byte, 1)
	n, err := r.Read(peekBuf)
	if err != nil {
		if err == io.EOF {
			return err
		}
		return fmt.Errorf("error reading message type: %v", err)
	}
	if n != 1 {
		return fmt.Errorf("unexpected number of bytes read: %d", n)
	}

	// Handle stream messages
	if peekBuf[0] == IncomingStream {
		msg.Stream = true
		return nil
	}

	// For regular messages, read the entire payload
	buf := make([]byte, 4096)
	n, err = r.Read(buf)
	if err != nil && err != io.EOF {
		return fmt.Errorf("error reading message payload: %v", err)
	}

	// Include the first byte we peeked at
	msg.Payload = append([]byte{peekBuf[0]}, buf[:n]...)
	return nil
}
