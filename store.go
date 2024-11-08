package main

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const ChunkSize = 1024 * 1024

type Metadata struct {
	FileID        string
	NumChunks     int
	FileExtension string
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

// ChunkAndStore divides a file into chunks, stores each chunk,
// and saves metadata about the number of chunks and file ID.

// func (s *Store) ChunkAndStore(fileID, filePath string) (*Metadata, error) {
// 	file, err := os.Open(filePath)
// 	if err != nil {
// 		return nil, err
// 	}
// 	defer file.Close()
//
// 	// Extract the file extension from the file path
// 	fileExtension := filepath.Ext(filePath)
//
// 	chunkIndex := 0
// 	for {
// 		buf := make([]byte, ChunkSize)
// 		n, err := file.Read(buf)
// 		if err == io.EOF {
// 			break
// 		}
// 		if err != nil {
// 			return nil, err
// 		}
// 		chunkFile := fmt.Sprintf("%s_chunk_%d", fileID, chunkIndex)
// 		if _, err := s.writeChunk(fileID, chunkFile, buf[:n]); err != nil {
// 			return nil, err
// 		}
// 		chunkIndex++
// 	}
//
// 	// Save metadata, including the file extension
// 	meta := &Metadata{
// 		FileID:        fileID,
// 		NumChunks:     chunkIndex,
// 		FileExtension: fileExtension,
// 	}
// 	if err := s.saveMetadata(meta); err != nil {
// 		return nil, err
// 	}
// 	return meta, nil
// }

func (s *Store) ChunkAndStore(fileID, filePath string) (*Metadata, error) {
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
		if _, err := s.writeChunk(fileID, chunkFileName, buf[:n]); err != nil {
			return nil, fmt.Errorf("failed to write chunk: %v", err)
		}
		chunkIndex++
	}

	// Create metadata to store the file details
	meta := &Metadata{
		FileID:        fileID,
		NumChunks:     chunkIndex,
		FileExtension: fileExtension,
	}

	// Save metadata as JSON in the store
	if err := s.saveMetadata(meta); err != nil {
		return nil, fmt.Errorf("failed to save metadata: %v", err)
	}

	return meta, nil
}


// saveMetadata saves metadata as a JSON file to the store directory
func (s *Store) saveMetadata(meta *Metadata) error {
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
func (s *Store) GetMetadata(fileID string) (*Metadata, error) {
	metaFile := fmt.Sprintf("%s/%s_metadata.json", s.opts.Root, fileID)
	file, err := os.Open(metaFile)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var metadata Metadata
	if err := json.NewDecoder(file).Decode(&metadata); err != nil {
		return nil, err
	}

	return &metadata, nil
}

// writeChunk saves a chunk of data to a file in the store directory
func (s *Store) writeChunk(id, name string, data []byte) (int, error) {
	dir := fmt.Sprintf("%s/%s", s.opts.Root, id)
	if err := os.MkdirAll(dir, os.ModePerm); err != nil {
		return 0, err
	}
	f, err := os.Create(fmt.Sprintf("%s/%s", dir, name))
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return f.Write(data)
}
