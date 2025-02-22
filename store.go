package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"encoding/gob"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
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
func (s *FileServer) handleChunkRequest(peer p2p.Peer, req p2p.MessageChunkRequest) error {
	log.Printf("Processing chunk request for file %s chunk %d", req.FileID, req.Chunk)

	// First check if we're actively sharing this file
	s.activeFilesMu.RLock()
	activeMeta, isActive := s.activeFiles[req.FileID]
	s.activeFilesMu.RUnlock()

	if !isActive {
		return fmt.Errorf("file %s is not actively shared by this peer", req.FileID)
	}

	// Add flow control - check if we're already sending too many chunks
	const maxConcurrentChunks = 5
	s.mu.Lock()
	activeSends := len(s.responseHandlers)
	s.mu.Unlock()
	if activeSends >= maxConcurrentChunks {
		return fmt.Errorf("too many active chunk transfers")
	}

	// Validate chunk index
	if req.Chunk >= activeMeta.NumChunks {
		return fmt.Errorf("invalid chunk index %d, file only has %d chunks",
			req.Chunk, activeMeta.NumChunks)
	}

	// Read chunk with retry logic
	var chunkData []byte
	var err error
	for retries := 0; retries < 3; retries++ {
		// Important: Use the OriginalPath from activeMeta, not directly from disk
		chunkData, err = s.store.ReadChunk(activeMeta.OriginalPath, req.Chunk)
		if err == nil {
			break
		}
		log.Printf("Error reading chunk (attempt %d/3): %v", retries+1, err)
		time.Sleep(time.Duration(retries+1) * 100 * time.Millisecond)
	}
	if err != nil {
		return fmt.Errorf("failed to read chunk after retries: %v", err)
	}

	// Send chunk with chunked transfer
	msg := p2p.NewChunkResponseMessage(req.FileID, req.Chunk, chunkData)
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(msg); err != nil {
		return fmt.Errorf("failed to encode response: %v", err)
	}

	// Split large messages into smaller chunks for transmission
	const maxChunkSize = 1024 * 1024 // 1MB transmission chunks
	data := buf.Bytes()
	totalLen := uint32(len(data))

	// First send the total message length
	if err := binary.Write(peer, binary.BigEndian, totalLen); err != nil {
		return fmt.Errorf("failed to write message length: %v", err)
	}

	// Then send data in chunks
	for offset := 0; offset < len(data); offset += maxChunkSize {
		end := offset + maxChunkSize
		if end > len(data) {
			end = len(data)
		}

		n, err := peer.Write(data[offset:end])
		if err != nil {
			return fmt.Errorf("failed to write payload: %v", err)
		}
		if n != end-offset {
			return fmt.Errorf("incomplete write: wrote %d of %d bytes", n, end-offset)
		}
	}

	log.Printf("Successfully sent chunk %d (%d bytes)", req.Chunk, len(chunkData))
	return nil
}

func (s *Store) ReadChunk(filePath string, chunkIndex int) ([]byte, error) {
	// Construct full path relative to store root
	fullPath := filepath.Join(s.opts.Root, filePath)

	file, err := os.OpenFile(fullPath, os.O_RDONLY, 0644)
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
	// Create a subdirectory for this file
	fileDir := filepath.Join(s.opts.Root, fileID)
	if err := os.MkdirAll(fileDir, os.ModePerm); err != nil {
		return nil, fmt.Errorf("failed to create directory: %v", err)
	}

	// Copy the file into the store
	storePath := filepath.Join(fileID, filepath.Base(filePath))
	destPath := filepath.Join(s.opts.Root, storePath)

	src, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open source file: %v", err)
	}
	defer src.Close()

	dst, err := os.Create(destPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create destination file: %v", err)
	}
	defer dst.Close()

	if _, err := io.Copy(dst, src); err != nil {
		return nil, fmt.Errorf("failed to copy file: %v", err)
	}

	// Create metadata
	meta := &shared.Metadata{
		FileID:        fileID,
		FileExtension: filepath.Ext(filePath),
		OriginalPath:  storePath, // Store relative path from store root
		ChunkSize:     ChunkSize,
	}

	// Calculate chunks and hashes
	chunkHashes, totalHash, totalSize, err := generateFileHashes(destPath, ChunkSize)
	if err != nil {
		return nil, fmt.Errorf("failed to generate hashes: %v", err)
	}

	meta.ChunkHashes = chunkHashes
	meta.TotalHash = totalHash
	meta.TotalSize = totalSize
	meta.NumChunks = len(chunkHashes)

	// Save metadata
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
