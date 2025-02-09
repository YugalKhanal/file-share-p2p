package p2p

import (
	"encoding/binary"
	"encoding/gob"
	"fmt"
	"io"
)

const ChunkSize = 16 * 1024 * 1024 //16MB chunks

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
	// Read message length
	var lengthBuf [4]byte
	if _, err := io.ReadFull(r, lengthBuf[:]); err != nil {
		return fmt.Errorf("failed to read message length: %v", err)
	}
	length := binary.BigEndian.Uint32(lengthBuf[:])

	// Handle empty messages (heartbeats)
	if length == 0 {
		return nil
	}

	// Basic sanity check - max message size is 17MB (16MB chunk + 1MB overhead)
	const maxMessageSize = 17 * 1024 * 1024
	if length > maxMessageSize {
		return fmt.Errorf("message too large: %d bytes (max: %d)", length, maxMessageSize)
	}

	// Read the complete message
	msg.Payload = make([]byte, length)
	_, err := io.ReadFull(r, msg.Payload)
	if err != nil {
		return fmt.Errorf("failed to read message: %v", err)
	}

	return nil
}
