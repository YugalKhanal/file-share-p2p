package p2p

import (
	"encoding/gob"
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
	if _, err := r.Read(peekBuf); err != nil {
		return err
	}

	// Handle stream messages
	if peekBuf[0] == IncomingStream {
		msg.Stream = true
		return nil
	}

	// For regular messages, read the entire payload
	buf := make([]byte, 4096) // Increased buffer size
	n, err := r.Read(buf)
	if err != nil && err != io.EOF {
		return err
	}

	// Include the first byte we peeked at
	msg.Payload = append([]byte{peekBuf[0]}, buf[:n]...)
	return nil
}
