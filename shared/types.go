package shared

import "path/filepath"

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

func (m *Metadata) FullPath(basePath string) string {
	return filepath.Join(basePath, m.FileID+"_metadata.json")
}
