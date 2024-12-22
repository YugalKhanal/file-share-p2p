package p2p

import (
	"encoding/gob"

	"github.com/anthdm/foreverstore/shared"
)

const (
	IncomingMessage = 0x1
	IncomingStream  = 0x2
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
	Payload interface{} // Allows various message types like chunk and metadata requests
}

func init() {
	// Register message types only once
	gob.RegisterName("p2p.Message", Message{})
	gob.RegisterName("p2p.MessageChunkRequest", MessageChunkRequest{})
	gob.RegisterName("p2p.MessageChunkResponse", MessageChunkResponse{})
	gob.RegisterName("p2p.MessageMetadataRequest", MessageMetadataRequest{})
	gob.RegisterName("p2p.MessageMetadataResponse", MessageMetadataResponse{})
}
