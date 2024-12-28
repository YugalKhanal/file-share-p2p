package main

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// calculateHash generates a SHA-1 hash of the provided data
func calculateHash(data []byte) string {
	hash := sha1.New()
	hash.Write(data)
	return hex.EncodeToString(hash.Sum(nil))
}

// verifyChunk checks if a chunk's hash matches the expected hash
func verifyChunk(data []byte, expectedHash string) bool {
	actualHash := calculateHash(data)
	return actualHash == expectedHash
}

// generateFileHashes generates hashes for each chunk and the total file
func generateFileHashes(filePath string, chunkSize int) ([]string, string, int64, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, "", 0, fmt.Errorf("failed to open file: %v", err)
	}
	defer file.Close()

	fileInfo, err := file.Stat()
	if err != nil {
		return nil, "", 0, fmt.Errorf("failed to get file info: %v", err)
	}

	totalSize := fileInfo.Size()
	numChunks := (totalSize + int64(chunkSize) - 1) / int64(chunkSize)
	chunkHashes := make([]string, int(numChunks))

	// Calculate total file hash
	file.Seek(0, 0)
	totalHasher := sha1.New()
	if _, err := io.Copy(totalHasher, file); err != nil {
		return nil, "", 0, fmt.Errorf("failed to calculate total hash: %v", err)
	}
	totalHash := hex.EncodeToString(totalHasher.Sum(nil))

	// Calculate chunk hashes
	file.Seek(0, 0)
	buf := make([]byte, chunkSize)
	for i := int64(0); i < numChunks; i++ {
		n, err := file.Read(buf)
		if err != nil && err != io.EOF {
			return nil, "", 0, fmt.Errorf("failed to read chunk: %v", err)
		}
		chunkHashes[i] = calculateHash(buf[:n])
	}

	return chunkHashes, totalHash, totalSize, nil
}
