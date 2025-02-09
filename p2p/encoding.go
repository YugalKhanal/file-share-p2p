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

	// More strict message size validation
	const maxMessageSize = 16 * 1024 * 1024 // 16MB max per message
	if length == 0 || length > maxMessageSize {
		return fmt.Errorf("invalid message length: %d (max allowed: %d)", length, maxMessageSize)
	}

	// Read the payload with a buffer
	payload := make([]byte, 0, length)
	buffer := make([]byte, 64*1024) // 64KB chunks for better network behavior

	bytesRead := uint32(0)
	for bytesRead < length {
		remaining := length - bytesRead
		if remaining < uint32(len(buffer)) {
			buffer = buffer[:remaining]
		}

		n, err := io.ReadFull(r, buffer)
		if err != nil {
			if err == io.ErrUnexpectedEOF {
				return fmt.Errorf("connection closed while reading payload (got %d of %d bytes)", bytesRead+uint32(n), length)
			}
			return fmt.Errorf("failed to read payload: %v", err)
		}

		payload = append(payload, buffer[:n]...)
		bytesRead += uint32(n)
	}

	msg.Payload = payload
	return nil
}
