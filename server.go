package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/gob"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/anthdm/foreverstore/p2p"
	"github.com/anthdm/foreverstore/shared"
	// "github.com/anthdm/foreverstore/shared"
)

type FileServerOpts struct {
	ListenAddr        string
	EncKey            []byte
	StorageRoot       string
	PathTransformFunc func(string) PathKey
	Transport         p2p.Transport
	TrackerAddr       string
	BootstrapNodes    []string
}

type FileServer struct {
	opts             FileServerOpts
	peers            map[string]p2p.Peer
	store            *Store
	quitch           chan struct{}
	mu               sync.RWMutex
	responseHandlers []func(p2p.Message)
}

func makeServer(listenAddr, bootstrapNode string) *FileServer {
	tcpOpts := p2p.TCPTransportOpts{
		ListenAddr:    listenAddr,
		HandshakeFunc: p2p.NOPHandshakeFunc,
		Decoder:       p2p.DefaultDecoder{},
	}
	transport := p2p.NewTCPTransport(tcpOpts)

	opts := FileServerOpts{
		ListenAddr: listenAddr,
		// ListenAddr:        "localhost:3000",
		EncKey:            newEncryptionKey(),
		StorageRoot:       "shared_files",
		PathTransformFunc: DefaultPathTransformFunc,
		Transport:         transport,
		BootstrapNodes:    []string{bootstrapNode},
	}

	server := &FileServer{
		opts:   opts,
		store:  NewStore(StoreOpts{Root: opts.StorageRoot, PathTransformFunc: opts.PathTransformFunc}),
		quitch: make(chan struct{}),
		peers:  make(map[string]p2p.Peer),
	}
	transport.SetOnPeer(server.onPeer)
	return server
}

// Fetches the public IP address of the server
func getPublicIP() (string, error) {
	resp, err := http.Get("http://checkip.amazonaws.com")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	ip, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(string(ip)), nil
}

// Fetches the local IP address of the server
func getLocalIP() (string, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", err
	}

	for _, addr := range addrs {
		if ipNet, ok := addr.(*net.IPNet); ok && !ipNet.IP.IsLoopback() && ipNet.IP.To4() != nil {
			return ipNet.IP.String(), nil
		}
	}
	return "", fmt.Errorf("no local IP found")
}

// Determines the peer's full address, combining the detected IP with the listening port
// func getPeerAddress(port string) (string, error) {
// 	publicIP, err := getPublicIP()
// 	if err != nil {
// 		localIP, err := getLocalIP()
// 		if err != nil {
// 			return "", fmt.Errorf("failed to determine peer IP: %v", err)
// 		}
// 		return fmt.Sprintf("%s:%s", localIP, port), nil
// 	}
// 	return fmt.Sprintf("%s:%s", publicIP, port), nil
// }

func getPeerAddress(port string) (string, error) {
	// For local development, always use localhost
	if strings.HasPrefix(port, ":") {
		port = port[1:]
	}
	return fmt.Sprintf("localhost:%s", port), nil
}

// New function to refresh peer list from tracker
func (s *FileServer) refreshPeers(fileID string) error {
	url := fmt.Sprintf("%s/peers?file_id=%s", s.opts.TrackerAddr, fileID)
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("failed to get peers from tracker: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("no peers found for file ID %s", fileID)
	}

	var peerList []string
	if err := json.NewDecoder(resp.Body).Decode(&peerList); err != nil {
		return fmt.Errorf("failed to decode peer list: %v", err)
	}

	log.Printf("Received peer list from tracker: %v", peerList)

	// Create a wait group to track connection attempts
	var wg sync.WaitGroup
	successChan := make(chan bool, len(peerList))

	// Try to connect to each peer
	for _, peerAddr := range peerList {
		if strings.HasSuffix(peerAddr, s.opts.ListenAddr) {
			log.Printf("Skipping own address: %s", peerAddr)
			continue
		}

		wg.Add(1)
		go func(addr string) {
			defer wg.Done()

			log.Printf("Attempting to connect to peer: %s", addr)
			if err := s.opts.Transport.Dial(addr); err != nil {
				log.Printf("Failed to connect to peer %s: %v", addr, err)
				return
			}

			// Wait briefly for the connection to be established
			time.Sleep(time.Second)

			// Verify connection was successful
			s.mu.RLock()
			_, connected := s.peers[p2p.NormalizeAddress(addr)]
			s.mu.RUnlock()

			if connected {
				log.Printf("Successfully connected to peer: %s", addr)
				successChan <- true
			}
		}(peerAddr)
	}

	// Wait for all connection attempts
	go func() {
		wg.Wait()
		close(successChan)
	}()

	// Wait up to 5 seconds for at least one successful connection
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()

	select {
	case _, ok := <-successChan:
		if ok {
			// Log current peers
			s.mu.RLock()
			log.Printf("Connected peers:")
			for addr := range s.peers {
				log.Printf("- %s", addr)
			}
			s.mu.RUnlock()
			return nil
		}
	case <-timer.C:
		return fmt.Errorf("timeout waiting for peer connections")
	}

	return fmt.Errorf("failed to establish any peer connections")
}

func (s *FileServer) Start() error {
	log.Printf("Starting server on %s", s.opts.ListenAddr)

	// Start consuming RPC messages in a separate goroutine
	go s.consumeRPCMessages()

	if err := s.opts.Transport.ListenAndAccept(); err != nil {
		return err
	}

	s.bootstrapNetwork()
	return nil
}

// New method to consume RPC messages
func (s *FileServer) consumeRPCMessages() {
	for rpc := range s.opts.Transport.Consume() {
		// Handle the RPC message
		if err := s.handleRPCMessage(rpc); err != nil {
			log.Printf("Error handling RPC message: %v", err)
		}
	}
}

func (s *FileServer) downloadChunk(fileID string, chunkIndex, chunkSize int, output *os.File) error {
	log.Printf("Starting chunk download for file %s, chunk %d", fileID, chunkIndex)

	meta, err := s.store.GetMetadata(fileID)
	if err != nil {
		return fmt.Errorf("failed to get metadata: %v", err)
	}

	fileSize := meta.TotalSize
	startOffset := int64(chunkIndex * chunkSize)
	if startOffset >= fileSize {
		return fmt.Errorf("chunk index %d is beyond file size", chunkIndex)
	}

	endOffset := startOffset + int64(chunkSize)
	if endOffset > fileSize {
		endOffset = fileSize
	}
	expectedSize := endOffset - startOffset

	s.mu.RLock()
	peers := make([]p2p.Peer, 0, len(s.peers))
	for addr, peer := range s.peers {
		log.Printf("Available peer: %s (remote: %s)", addr, peer.RemoteAddr())
		peers = append(peers, peer)
	}
	s.mu.RUnlock()

	if len(peers) == 0 {
		return fmt.Errorf("no peers available")
	}

	// Increased timeout for large chunks
	timeout := 2 * time.Minute

	// Try downloading from each peer
	for _, peer := range peers {
		log.Printf("Attempting to download chunk %d from peer %s", chunkIndex, peer.RemoteAddr())

		responseChan := make(chan p2p.MessageChunkResponse, 1)

		// Set up response handler for this attempt
		s.mu.Lock()
		handlerIndex := len(s.responseHandlers)
		s.responseHandlers = append(s.responseHandlers, func(msg p2p.Message) {
			if msg.Type != p2p.MessageTypeChunkResponse {
				return
			}

			var resp p2p.MessageChunkResponse
			switch payload := msg.Payload.(type) {
			case p2p.MessageChunkResponse:
				resp = payload
			case *p2p.MessageChunkResponse:
				resp = *payload
			default:
				log.Printf("Invalid chunk response payload type: %T", msg.Payload)
				return
			}

			if resp.FileID != fileID || resp.Chunk != chunkIndex {
				return
			}

			select {
			case responseChan <- resp:
			default:
			}
		})
		s.mu.Unlock()

		// Send request
		msg := p2p.NewChunkRequestMessage(fileID, chunkIndex)
		var buf bytes.Buffer
		if err := gob.NewEncoder(&buf).Encode(msg); err != nil {
			log.Printf("Error encoding request: %v", err)
			continue
		}

		if err := peer.Send(buf.Bytes()); err != nil {
			log.Printf("Error sending request to peer %s: %v", peer.RemoteAddr(), err)
			continue
		}

		// Wait for response with timeout
		select {
		case resp := <-responseChan:
			// Clean up handler
			s.mu.Lock()
			if handlerIndex < len(s.responseHandlers) {
				s.responseHandlers = append(s.responseHandlers[:handlerIndex], s.responseHandlers[handlerIndex+1:]...)
			}
			s.mu.Unlock()
			close(responseChan)

			if int64(len(resp.Data)) > expectedSize {
				log.Printf("Received oversized chunk data from peer %s", peer.RemoteAddr())
				continue
			}

			if chunkIndex < len(meta.ChunkHashes) {
				if !verifyChunk(resp.Data, meta.ChunkHashes[chunkIndex]) {
					log.Printf("Chunk verification failed for chunk %d from peer %s", chunkIndex, peer.RemoteAddr())
					continue
				}
			}

			written, err := output.WriteAt(resp.Data, startOffset)
			if err != nil {
				log.Printf("Error writing chunk data: %v", err)
				continue
			}

			if int64(written) != int64(len(resp.Data)) {
				log.Printf("Incomplete chunk write: wrote %d of %d bytes", written, len(resp.Data))
				continue
			}

			log.Printf("Successfully wrote verified chunk %d (%d bytes)", chunkIndex, written)
			return nil

		case <-time.After(timeout):
			// Clean up handler
			s.mu.Lock()
			if handlerIndex < len(s.responseHandlers) {
				s.responseHandlers = append(s.responseHandlers[:handlerIndex], s.responseHandlers[handlerIndex+1:]...)
			}
			s.mu.Unlock()
			close(responseChan)

			log.Printf("Timeout waiting for response from peer %s", peer.RemoteAddr())
			continue
		}
	}

	return fmt.Errorf("failed to download chunk %d from any peer", chunkIndex)
}

// New method to handle RPC messages
func (s *FileServer) handleRPCMessage(rpc p2p.RPC) error {
	// Decode the basic message structure
	var msg p2p.Message
	decoder := gob.NewDecoder(bytes.NewReader(rpc.Payload))
	if err := decoder.Decode(&msg); err != nil {
		return fmt.Errorf("decode error: %v", err)
	}

	// Get peer information
	normalizedAddr := p2p.NormalizeAddress(rpc.From)
	s.mu.RLock()
	peer, exists := s.peers[normalizedAddr]
	s.mu.RUnlock()

	if !exists {
		return fmt.Errorf("unknown peer: %s", rpc.From)
	}

	switch msg.Type {
	case p2p.MessageTypeChunkRequest:
		// Try both pointer and value type assertions
		switch req := msg.Payload.(type) {
		case p2p.MessageChunkRequest:
			return s.handleChunkRequest(peer, req)
		case *p2p.MessageChunkRequest:
			return s.handleChunkRequest(peer, *req)
		default:
			return fmt.Errorf("invalid chunk request payload type: %T", msg.Payload)
		}

	case p2p.MessageTypeChunkResponse:
		s.mu.RLock()
		for _, handler := range s.responseHandlers {
			handler(msg)
		}
		s.mu.RUnlock()
		return nil

	default:
		return fmt.Errorf("unknown message type: %s", msg.Type)
	}
}

// Add a helper method to list current peers (for debugging)
func (s *FileServer) listPeers() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	peers := make([]string, 0, len(s.peers))
	for addr := range s.peers {
		peers = append(peers, addr)
	}
	return peers
}

func (s *FileServer) onPeer(peer p2p.Peer) error {
	// Get the original address
	origAddr := peer.RemoteAddr().String()
	log.Printf("New peer connection from: %s", origAddr)

	// Use the same normalization function
	addr := p2p.NormalizeAddress(origAddr)

	s.mu.Lock()
	s.peers[addr] = peer
	peerCount := len(s.peers)
	s.mu.Unlock()

	log.Printf("Connected to peer %s (normalized from %s) (total peers: %d)",
		addr, origAddr, peerCount)
	return nil
}

func (s *FileServer) ShareFile(filePath string) error {
	fileID, err := s.generateFileID(filePath)
	if err != nil {
		return fmt.Errorf("failed to generate file ID: %v", err)
	}

	// Generate chunk hashes and total file hash
	chunkHashes, totalHash, totalSize, err := generateFileHashes(filePath, ChunkSize)
	if err != nil {
		return fmt.Errorf("failed to generate hashes: %v", err)
	}

	meta := &shared.Metadata{
		FileID:        fileID,
		NumChunks:     len(chunkHashes),
		ChunkSize:     ChunkSize,
		FileExtension: filepath.Ext(filePath),
		OriginalPath:  filePath,
		ChunkHashes:   chunkHashes,
		TotalHash:     totalHash,
		TotalSize:     totalSize,
	}

	if err := s.store.saveMetadata(meta); err != nil {
		return fmt.Errorf("failed to save metadata: %v", err)
	}

	if s.opts.TrackerAddr != "" {
		if err := announceToTracker(s.opts.TrackerAddr, fileID, s.opts.ListenAddr); err != nil {
			log.Printf("Failed to announce to tracker: %v", err)
		}
	}

	log.Printf("File %s shared with ID %s and %d chunks", filePath, fileID, len(chunkHashes))
	return nil
}

func (s *FileServer) generateFileID(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	hash := sha1.New() // or use sha256.New() for SHA-256
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}

	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (s *FileServer) logPeerState() {
	s.mu.RLock()
	defer s.mu.RUnlock()

	log.Printf("Current peer state:")
	for addr, peer := range s.peers {
		log.Printf("  - Peer %s (remote addr: %s)", addr, peer.RemoteAddr())
	}
}

func (s *FileServer) DownloadFile(fileID string) error {
	log.Printf("Attempting to download file with ID: %s", fileID)

	if err := s.refreshPeers(fileID); err != nil {
		return fmt.Errorf("failed to get peers from tracker: %v", err)
	}

	// Wait a bit for connections to establish
	time.Sleep(2 * time.Second)

	meta, err := s.store.GetMetadata(fileID)
	if err != nil {
		return fmt.Errorf("failed to get metadata: %v", err)
	}

	// Initialize piece manager
	pieceManager := NewPieceManager(meta.NumChunks, meta.ChunkHashes)

	// Initialize piece availability for each peer
	s.mu.RLock()
	for addr := range s.peers {
		pieces := make([]int, meta.NumChunks)
		for i := 0; i < meta.NumChunks; i++ {
			pieces[i] = i
		}
		pieceManager.UpdatePeerPieces(addr, pieces)
	}
	s.mu.RUnlock()

	outputFileName := fmt.Sprintf("downloaded_%s%s", fileID, meta.FileExtension)
	outputFile, err := os.Create(outputFileName)
	if err != nil {
		return fmt.Errorf("failed to create output file: %v", err)
	}
	defer outputFile.Close()

	// Create semaphore to limit concurrent downloads
	const maxConcurrent = 5
	sem := make(chan struct{}, maxConcurrent)

	// Create error channel with buffer to prevent goroutine leaks
	errorChan := make(chan error, meta.NumChunks)
	var wg sync.WaitGroup

	// Download chunks with controlled concurrency
	for !pieceManager.IsComplete() {
		pieces := pieceManager.GetNextPieces(maxConcurrent)
		if len(pieces) == 0 {
			break
		}

		for _, piece := range pieces {
			wg.Add(1)
			sem <- struct{}{} // Acquire semaphore

			go func(p PieceInfo) {
				defer wg.Done()
				defer func() { <-sem }() // Release semaphore

				// Try to download the piece with retries and backoff
				var lastErr error
				for retries := 0; retries < 3; retries++ {
					// Exponential backoff between retries
					if retries > 0 {
						backoff := time.Duration(retries) * 500 * time.Millisecond
						time.Sleep(backoff)
					}

					if err := s.downloadChunk(fileID, p.Index, meta.ChunkSize, outputFile); err != nil {
						lastErr = err
						pieceManager.MarkPieceStatus(p.Index, PieceMissing)
						continue
					}

					pieceManager.MarkPieceStatus(p.Index, PieceVerified)
					return
				}

				errorChan <- fmt.Errorf("failed to download piece %d after retries: %v", p.Index, lastErr)
			}(piece)
		}

		// Wait for the current batch to complete before requesting more
		wg.Wait()

		// Check for any errors from the batch
		select {
		case err := <-errorChan:
			os.Remove(outputFileName)
			return fmt.Errorf("download failed: %v", err)
		default:
			// No errors, continue with next batch
		}
	}

	// Final verification
	if err := s.verifyDownloadedFile(outputFileName, meta.TotalHash); err != nil {
		os.Remove(outputFileName)
		return fmt.Errorf("file verification failed: %v", err)
	}

	log.Printf("Successfully downloaded and verified file %s", outputFileName)
	return nil
}

func (s *FileServer) verifyDownloadedFile(filePath string, expectedHash string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open downloaded file: %v", err)
	}
	defer file.Close()

	hasher := sha1.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return fmt.Errorf("failed to calculate file hash: %v", err)
	}

	actualHash := hex.EncodeToString(hasher.Sum(nil))
	if actualHash != expectedHash {
		return fmt.Errorf("file verification failed: hash mismatch")
	}

	return nil
}

func (s *FileServer) handleMessage(peer p2p.Peer, msg *p2p.Message) error {
	switch msg.Type {
	case "chunk_request":
		payload, ok := msg.Payload.(p2p.MessageChunkRequest)
		if !ok {
			return fmt.Errorf("invalid payload type for chunk_request")
		}
		return s.handleChunkRequest(peer, payload)
	case "metadata_request":
		payload, ok := msg.Payload.(p2p.MessageMetadataRequest)
		if !ok {
			return fmt.Errorf("invalid payload type for metadata_request")
		}
		return s.handleMetadataRequest(peer, payload)
	default:
		log.Printf("Received unknown message type %s from %s", msg.Type, peer.RemoteAddr())
		return nil
	}
}

func (s *FileServer) handleMetadataRequest(peer p2p.Peer, req p2p.MessageMetadataRequest) error {
	log.Printf("Received metadata request for file ID %s from peer %s", req.FileID, peer.RemoteAddr())

	// Load metadata
	meta, err := s.store.GetMetadata(req.FileID)
	if err != nil {
		log.Printf("Metadata not found for file ID %s: %v", req.FileID, err)
		return err
	}

	// Send metadata back to the requesting peer
	resp := p2p.MessageMetadataResponse{Metadata: *meta}
	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	if err := enc.Encode(resp); err != nil {
		log.Printf("Failed to encode metadata response: %v", err)
		return err
	}

	if _, err := peer.Write(buf.Bytes()); err != nil {
		log.Printf("Failed to send metadata response to peer %s: %v", peer.RemoteAddr(), err)
		return err
	}

	log.Printf("Sent metadata for file ID %s to peer %s", req.FileID, peer.RemoteAddr())
	return nil
}

func (s *FileServer) SetTrackerAddress(addr string) {
	s.opts.TrackerAddr = addr
}

func (s *FileServer) bootstrapNetwork() {
	for _, node := range s.opts.BootstrapNodes {
		if node != "" {
			go func(node string) {
				log.Printf("Connecting to bootstrap node %s", node)
				if err := s.opts.Transport.Dial(node); err != nil {
					log.Printf("Failed to connect to %s: %v", node, err)
				}
			}(node)
		}
	}
}

// announceToTracker sends a request to the tracker to announce this peer's file availability.
func announceToTracker(trackerAddr, fileID, listenAddr string) error {
	peerAddr, err := getPeerAddress(listenAddr) // No need to strip ":" anymore
	if err != nil {
		return fmt.Errorf("failed to determine peer address: %v", err)
	}

	url := fmt.Sprintf("%s/announce?file_id=%s&peer_addr=%s",
		trackerAddr, fileID, peerAddr)

	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to announce to tracker, status code: %d", resp.StatusCode)
	}

	log.Printf("Successfully announced to tracker at %s", trackerAddr)
	return nil
}
