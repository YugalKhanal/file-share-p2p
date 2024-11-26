package p2p

import "github.com/anthdm/foreverstore/shared"

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
