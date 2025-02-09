package p2p

import (
	"bytes"
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

	// Read message type first (small fixed header)
	headerBuf := make([]byte, 256) // Small buffer for message type/header
	n, err := io.ReadFull(r, headerBuf[:min(256, length)])
	if err != nil {
		return fmt.Errorf("failed to read message header: %v", err)
	}

	// Decode message type
	var msgType struct {
		Type string
	}
	if err := gob.NewDecoder(bytes.NewReader(headerBuf[:n])).Decode(&msgType); err != nil {
		return fmt.Errorf("failed to decode message type: %v", err)
	}

	// Validate size based on message type
	switch msgType.Type {
	case MessageTypeChunkRequest:
		if length > 1024 { // 1KB is more than enough for request
			return fmt.Errorf("chunk request too large: %d bytes", length)
		}
	case MessageTypeChunkResponse:
		expectedSize := ChunkSize + 1024 // 16MB chunk + 1KB overhead
		if length > uint32(expectedSize) {
			return fmt.Errorf("chunk response too large: %d bytes (max: %d)", length, expectedSize)
		}
	default:
		return fmt.Errorf("unknown message type: %s", msgType.Type)
	}

	// Read the rest of the message
	payload := make([]byte, length)
	copy(payload, headerBuf[:n])
	if length > uint32(n) {
		_, err = io.ReadFull(r, payload[n:])
		if err != nil {
			return fmt.Errorf("failed to read message body: %v", err)
		}
	}

	msg.Payload = payload
	return nil
}
