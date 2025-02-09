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
	// Read message length with timeout
	var length uint32
	if err := binary.Read(r, binary.BigEndian, &length); err != nil {
		return fmt.Errorf("failed to read message length: %v", err)
	}

	// Handle empty messages (heartbeats)
	if length == 0 {
		return nil
	}

	// Validate message size
	const maxMessageSize = 32 * 1024 * 1024 // 32MB max message size
	if length > maxMessageSize {
		return fmt.Errorf("message too large: %d bytes (max: %d)", length, maxMessageSize)
	}

	// Read the complete message with retries
	data := make([]byte, length)
	totalRead := 0
	for totalRead < int(length) {
		n, err := r.Read(data[totalRead:])
		if err != nil {
			if err == io.EOF && totalRead > 0 {
				return fmt.Errorf("incomplete message: got %d of %d bytes", totalRead, length)
			}
			return fmt.Errorf("failed to read message: %v", err)
		}
		totalRead += n
	}

	msg.Payload = data
	return nil
}
