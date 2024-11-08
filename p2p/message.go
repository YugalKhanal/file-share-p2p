package p2p

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
