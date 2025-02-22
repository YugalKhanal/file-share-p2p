package shared

type Metadata struct {
	FileID        string
	NumChunks     int
	ChunkSize     int
	FileExtension string
	OriginalPath  string
	ChunkHashes   []string
	TotalHash     string
	TotalSize     int64
}
