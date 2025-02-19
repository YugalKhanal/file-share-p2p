package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/gob"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/anthdm/foreverstore/p2p"
	"github.com/anthdm/foreverstore/shared"
)

const ChunkSize = 1024 * 1024 * 16 // 16MB chunks instead of 1MB

type Metadata struct {
	FileID        string
	NumChunks     int
	FileExtension string
	ChunkSize     int
}

type PathTransformFunc func(string) PathKey

type PathKey struct {
	PathName string
	Filename string
}

func (p PathKey) FullPath() string {
	return fmt.Sprintf("%s/%s", p.PathName, p.Filename)
}

func CASPathTransformFunc(key string) PathKey {
	hash := sha1.Sum([]byte(key))
	hashStr := hex.EncodeToString(hash[:])
	return PathKey{
		PathName: hashStr[:5],
		Filename: hashStr,
	}
}

var DefaultPathTransformFunc = CASPathTransformFunc

type StoreOpts struct {
	Root              string
	PathTransformFunc PathTransformFunc
}

type Store struct {
	opts StoreOpts
}

func NewStore(opts StoreOpts) *Store {
	if opts.PathTransformFunc == nil {
		opts.PathTransformFunc = DefaultPathTransformFunc
	}
	if len(opts.Root) == 0 {
		opts.Root = "store"
	}
	return &Store{opts: opts}
}

// handleChunkRequest reads a chunk file from disk and sends it to the requesting peer.
const UDPChunkSize = 60 * 1024 // 60KB chunks for UDP

func (s *FileServer) handleChunkRequest(peer p2p.Peer, req p2p.MessageChunkRequest) error {
	meta, err := s.store.GetMetadata(req.FileID)
	if err != nil {
		return err
	}

	chunkData, err := s.store.ReadChunk(meta.OriginalPath, req.Chunk)
	if err != nil {
		return err
	}

	// Split into smaller UDP-friendly chunks
	for i := 0; i < len(chunkData); i += UDPChunkSize {
		end := i + UDPChunkSize
		if end > len(chunkData) {
			end = len(chunkData)
		}

		msg := p2p.NewChunkResponseMessage(req.FileID, req.Chunk, chunkData[i:end])

		var buf bytes.Buffer
		if err := gob.NewEncoder(&buf).Encode(msg); err != nil {
			return err
		}

		if err := peer.Send(buf.Bytes()); err != nil {
			return err
		}

		// Small delay between UDP packets
		time.Sleep(10 * time.Millisecond)
	}

	return nil
}

func (s *Store) ReadChunk(filePath string, chunkIndex int) ([]byte, error) {
	file, err := os.OpenFile(filePath, os.O_RDONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open file: %v", err)
	}
	defer file.Close()

	// Get file size
	fileInfo, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("failed to get file info: %v", err)
	}

	// Calculate chunk boundaries
	startOffset := int64(chunkIndex * ChunkSize)
	if startOffset >= fileInfo.Size() {
		return nil, fmt.Errorf("chunk start offset exceeds file size")
	}

	endOffset := startOffset + int64(ChunkSize)
	if endOffset > fileInfo.Size() {
		endOffset = fileInfo.Size()
	}

	chunkSize := endOffset - startOffset

	// Use buffered reading
	chunk := make([]byte, chunkSize)
	_, err = file.ReadAt(chunk, startOffset)
	if err != nil && err != io.EOF {
		return nil, fmt.Errorf("failed to read chunk: %v", err)
	}

	return chunk, nil
}

// ChunkAndStore divides a file into chunks, stores each chunk,
// and saves metadata about the number of chunks and file ID.
func (s *Store) ChunkAndStore(fileID, filePath string) (*shared.Metadata, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open file: %v", err)
	}
	defer file.Close()

	// Extract the file extension from the file path
	fileExtension := filepath.Ext(filePath)

	// Variable to track the number of chunks created
	chunkIndex := 0

	// Read and store chunks
	for {
		buf := make([]byte, ChunkSize)
		n, err := file.Read(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read chunk: %v", err)
		}

		// Define the chunk file name based on the file ID and chunk index
		chunkFileName := fmt.Sprintf("%s_chunk_%d", fileID, chunkIndex)
		if _, err := s.writeChunk(fileID, chunkFileName, fileExtension, buf[:n]); err != nil {
			return nil, fmt.Errorf("failed to write chunk: %v", err)
		}
		chunkIndex++
	}

	// Create metadata to store the file details
	meta := &shared.Metadata{
		FileID:        fileID,
		NumChunks:     chunkIndex,
		FileExtension: fileExtension,
		ChunkSize:     ChunkSize,
	}

	// Save metadata as JSON in the store
	if err := s.saveMetadata(meta); err != nil {
		return nil, fmt.Errorf("failed to save metadata: %v", err)
	}

	return meta, nil
}

// saveMetadata saves metadata as a JSON file to the store directory
func (s *Store) saveMetadata(meta *shared.Metadata) error {
	// Create the directory if it doesn't exist
	metaDir := s.opts.Root
	if err := os.MkdirAll(metaDir, os.ModePerm); err != nil {
		return err
	}

	// Save the metadata file within the directory
	metaFile := fmt.Sprintf("%s/%s_metadata.json", metaDir, meta.FileID)
	file, err := os.Create(metaFile)
	if err != nil {
		return err
	}
	defer file.Close()

	return json.NewEncoder(file).Encode(meta)
}

// GetMetadata retrieves metadata for a file based on file ID
func (s *Store) GetMetadata(fileID string) (*shared.Metadata, error) {
	metaFile := fmt.Sprintf("%s/%s_metadata.json", s.opts.Root, fileID)
	file, err := os.Open(metaFile)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var metadata shared.Metadata
	if err := json.NewDecoder(file).Decode(&metadata); err != nil {
		return nil, err
	}

	return &metadata, nil
}

// writeChunk saves a chunk of data to a file in the store directory
func (s *Store) writeChunk(id, name, ext string, data []byte) (int, error) {
	dir := fmt.Sprintf("%s/%s", s.opts.Root, id)
	if err := os.MkdirAll(dir, os.ModePerm); err != nil {
		return 0, err
	}
	// Make sure we only have one dot before the extension
	name = strings.TrimSuffix(name, ".")
	filePath := fmt.Sprintf("%s/%s%s", dir, name, ext)
	f, err := os.Create(filePath)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return f.Write(data)
}
