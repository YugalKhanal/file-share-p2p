package p2p

import (
	"encoding/gob"
	"github.com/anthdm/foreverstore/shared"
)

const (
	IncomingMessage = 0x1
	IncomingStream  = 0x2

	// Message types as constants to avoid string literals
	MessageTypeChunkRequest    = "chunk_request"
	MessageTypeChunkResponse   = "chunk_response"
	MessageTypeMetadataRequest = "metadata_request"
)

type RPC struct {
	From    string
	Payload []byte
	Stream  bool
}

type MessageChunkRequest struct {
	FileID string
	Chunk  int
}

type MessageChunkResponse struct {
	FileID string
	Chunk  int
	Data   []byte
}

type MessageMetadataRequest struct {
	FileID string
}

type MessageMetadataResponse struct {
	Metadata shared.Metadata
}

type Message struct {
	Type    string
	Payload interface{}
}

// Register message types for gob encoding
func init() {
	// Use RegisterName to avoid package path issues
	gob.RegisterName("Message", Message{})
	gob.RegisterName("MessageChunkRequest", MessageChunkRequest{})
	gob.RegisterName("MessageChunkResponse", MessageChunkResponse{})
	gob.RegisterName("MessageMetadataRequest", MessageMetadataRequest{})
	gob.RegisterName("MessageMetadataResponse", MessageMetadataResponse{})
	gob.RegisterName("Metadata", shared.Metadata{})
}

// Helper functions to ensure consistent message creation
func NewChunkRequestMessage(fileID string, chunk int) Message {
	return Message{
		Type: MessageTypeChunkRequest,
		Payload: MessageChunkRequest{
			FileID: fileID,
			Chunk:  chunk,
		},
	}
}

func NewChunkResponseMessage(fileID string, chunk int, data []byte) Message {
	return Message{
		Type: MessageTypeChunkResponse,
		Payload: MessageChunkResponse{
			FileID: fileID,
			Chunk:  chunk,
			Data:   data,
		},
	}
}
