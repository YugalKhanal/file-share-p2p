package shared

import "path/filepath"

type Metadata struct {
	FileID        string
	NumChunks     int
	FileExtension string
	ChunkSize     int
	OriginalPath  string
}

func (m *Metadata) FullPath(basePath string) string {
	return filepath.Join(basePath, m.FileID+"_metadata.json")
}
