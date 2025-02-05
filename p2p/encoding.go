package p2p

import (
	"encoding/binary"
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
	// Read message length with timeout
	var length uint32
	if err := binary.Read(r, binary.BigEndian, &length); err != nil {
		return fmt.Errorf("failed to read message length: %v", err)
	}

	// Sanity check for message length
	if length == 0 || length > 1024*1024*64 { // 64MB max message size
		return fmt.Errorf("invalid message length: %d", length)
	}

	// Read the payload with a buffer
	payload := make([]byte, 0, length)
	// buffer := make([]byte, 32*1024) // 32KB chunks
	buffer := make([]byte, 1024*1024) // 1MB chunks

	for uint32(len(payload)) < length {
		remaining := length - uint32(len(payload))
		if remaining < uint32(len(buffer)) {
			buffer = buffer[:remaining]
		}

		n, err := r.Read(buffer)
		if err != nil {
			return fmt.Errorf("failed to read payload: %v", err)
		}

		payload = append(payload, buffer[:n]...)
	}

	msg.Payload = payload
	return nil
}
