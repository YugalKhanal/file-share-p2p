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
	"math/rand/v2"
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
// Add this to the utility functions
func getPublicIP() (string, error) {
	resp, err := http.Get("https://api.ipify.org?format=text")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	ip, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	return string(ip), nil
}

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
func getPeerAddress(listenAddr string) (string, error) {
	// Get both public and local IPs
	publicIP, err := getPublicIP()
	if err != nil {
		return "", fmt.Errorf("failed to get public IP: %v", err)
	}

	localIP, err := getLocalIP()
	if err != nil {
		return "", fmt.Errorf("failed to get local IP: %v", err)
	}

	// Extract port from listen address
	port := listenAddr
	if strings.HasPrefix(port, ":") {
		port = port[1:]
	}

	// Return both addresses in a format that peers can use
	return fmt.Sprintf("%s:%s|%s:%s", publicIP, port, localIP, port), nil
}

// func getPeerAddress(port string) (string, error) {
// 	// For local development, always use localhost
// 	if strings.HasPrefix(port, ":") {
// 		port = port[1:]
// 	}
// 	return fmt.Sprintf("localhost:%s", port), nil
// }

// New function to refresh peer list from tracker
func (s *FileServer) refreshPeers(fileID string) error {
	url := fmt.Sprintf("%s/peers?file_id=%s", s.opts.TrackerAddr, fileID)
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("failed to get peers from tracker: %v", err)
	}
	defer resp.Body.Close()

	var peerList []string
	if err := json.NewDecoder(resp.Body).Decode(&peerList); err != nil {
		return fmt.Errorf("failed to decode peer list: %v", err)
	}

	log.Printf("Received peer list from tracker: %v", peerList)

	// Create a wait group to track connection attempts
	var wg sync.WaitGroup
	successChan := make(chan bool, len(peerList))

	myAddr := s.opts.ListenAddr
	if strings.HasPrefix(myAddr, ":") {
		myAddr = "localhost" + myAddr
	}

	// Try to connect to each peer
	for _, peerAddr := range peerList {
		addresses := strings.Split(peerAddr, "|")
		for _, addr := range addresses {
			if addr == myAddr {
				log.Printf("Skipping own address: %s", addr)
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

				s.mu.RLock()
				_, connected := s.peers[addr]
				s.mu.RUnlock()

				if connected {
					log.Printf("Successfully connected to peer: %s", addr)
					successChan <- true
				}
			}(addr)
		}
	}

	// Wait for all connection attempts
	go func() {
		wg.Wait()
		close(successChan)
	}()

	// Wait for at least one successful connection
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()

	select {
	case _, ok := <-successChan:
		if ok {
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
	// Reduce initial timeout for faster failure detection
	timeout := 30 * time.Second
	if chunkSize > 16*1024*1024 { // For chunks larger than 16MB
		timeout = time.Duration(chunkSize/(1024*1024)) * time.Second // Scale timeout with chunk size
	}

	s.mu.RLock()
	peers := make([]p2p.Peer, 0, len(s.peers))
	for _, peer := range s.peers {
		peers = append(peers, peer)
	}
	s.mu.RUnlock()

	if len(peers) == 0 {
		return fmt.Errorf("no peers available")
	}

	// Randomize peer selection to distribute load
	rand.Shuffle(len(peers), func(i, j int) {
		peers[i], peers[j] = peers[j], peers[i]
	})

	// Try each peer with individual timeouts
	for _, peer := range peers {
		responseChan := make(chan p2p.MessageChunkResponse, 1)
		errChan := make(chan error, 1)

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
				errChan <- fmt.Errorf("invalid payload type: %T", msg.Payload)
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

		// Send request in goroutine
		go func() {
			msg := p2p.NewChunkRequestMessage(fileID, chunkIndex)
			var buf bytes.Buffer
			if err := gob.NewEncoder(&buf).Encode(msg); err != nil {
				errChan <- err
				return
			}

			if err := peer.Send(buf.Bytes()); err != nil {
				errChan <- err
				return
			}
		}()

		// Wait for response with timeout
		select {
		case resp := <-responseChan:
			// Clean up handler
			s.mu.Lock()
			if handlerIndex < len(s.responseHandlers) {
				s.responseHandlers = append(s.responseHandlers[:handlerIndex], s.responseHandlers[handlerIndex+1:]...)
			}
			s.mu.Unlock()

			// Verify and write chunk
			if err := writeAndVerifyChunk(resp.Data, output, chunkIndex, chunkSize); err != nil {
				continue
			}
			return nil

		case err := <-errChan:
			log.Printf("Error downloading from peer %s: %v", peer.RemoteAddr(), err)
			continue

		case <-time.After(timeout):
			log.Printf("Timeout waiting for response from peer %s", peer.RemoteAddr())
			continue
		}
	}

	return fmt.Errorf("failed to download chunk %d from any peer", chunkIndex)
}

func writeAndVerifyChunk(data []byte, output *os.File, chunkIndex, chunkSize int) error {
	startOffset := int64(chunkIndex * chunkSize)

	// Verify chunk size
	if len(data) > chunkSize {
		return fmt.Errorf("oversized chunk data")
	}

	// Write chunk with retry
	for retries := 0; retries < 3; retries++ {
		written, err := output.WriteAt(data, startOffset)
		if err != nil {
			if retries < 2 {
				time.Sleep(time.Duration(retries+1) * 100 * time.Millisecond)
				continue
			}
			return fmt.Errorf("write error: %v", err)
		}

		if written != len(data) {
			if retries < 2 {
				continue
			}
			return fmt.Errorf("incomplete write: %d of %d bytes", written, len(data))
		}

		return nil
	}

	return fmt.Errorf("failed to write chunk after retries")
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

	hash := sha1.New()
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

	// Create error channel with buffer for all pieces to prevent goroutine leaks
	errorChan := make(chan error, meta.NumChunks)
	var wg sync.WaitGroup

	// Track completed pieces for progress reporting
	completedPieces := 0
	startTime := time.Now()
	lastProgressTime := startTime
	var progressMutex sync.Mutex

	// Progress reporting goroutine
	progressTicker := time.NewTicker(5 * time.Second)
	defer progressTicker.Stop()
	go func() {
		for range progressTicker.C {
			progressMutex.Lock()
			currentCompleted := completedPieces
			currentTime := time.Now()
			elapsed := currentTime.Sub(lastProgressTime).Seconds()
			if elapsed > 0 {
				piecesPerSecond := float64(currentCompleted-0) / elapsed
				remainingPieces := meta.NumChunks - currentCompleted
				eta := float64(remainingPieces) / piecesPerSecond
				log.Printf("Progress: %d/%d pieces (%.2f%%) - %.2f pieces/sec - ETA: %.1f seconds",
					currentCompleted, meta.NumChunks,
					float64(currentCompleted)/float64(meta.NumChunks)*100,
					piecesPerSecond, eta)
			}
			progressMutex.Unlock()
		}
	}()

	// Keep track of active download attempts
	activeDownloads := make(map[int]bool)
	var activeMutex sync.Mutex

	// Main download loop
	for !pieceManager.IsComplete() {
		pieces := pieceManager.GetNextPieces(maxConcurrent)
		if len(pieces) == 0 {
			// Check if we have any active downloads before breaking
			activeMutex.Lock()
			if len(activeDownloads) == 0 {
				activeMutex.Unlock()
				break
			}
			activeMutex.Unlock()
			time.Sleep(100 * time.Millisecond)
			continue
		}

		for _, piece := range pieces {
			wg.Add(1)
			sem <- struct{}{} // Acquire semaphore

			// Mark piece as being downloaded
			activeMutex.Lock()
			activeDownloads[piece.Index] = true
			activeMutex.Unlock()

			go func(p PieceInfo) {
				defer wg.Done()
				defer func() { <-sem }() // Release semaphore
				defer func() {
					activeMutex.Lock()
					delete(activeDownloads, p.Index)
					activeMutex.Unlock()
				}()

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
					progressMutex.Lock()
					completedPieces++
					progressMutex.Unlock()
					return
				}

				select {
				case errorChan <- fmt.Errorf("failed to download piece %d after retries: %v", p.Index, lastErr):
				default:
					// Channel is full or closed, log the error
					log.Printf("Error downloading piece %d: %v", p.Index, lastErr)
				}
			}(piece)
		}
	}

	// Wait for all downloads to complete
	wg.Wait()

	// Check for any errors after all downloads are complete
	select {
	case err := <-errorChan:
		if err != nil {
			os.Remove(outputFileName)
			return fmt.Errorf("download failed: %v", err)
		}
	default:
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
	peerAddr, err := getPeerAddress(listenAddr)
	if err != nil {
		return fmt.Errorf("failed to determine peer address: %v", err)
	}

	// Create announcement payload
	announcement := struct {
		FileID      string   `json:"file_id"`
		PeerAddr    string   `json:"peer_addr"`
		Name        string   `json:"name"`
		Size        int64    `json:"size"`
		Description string   `json:"description"`
		Categories  []string `json:"categories"`
		Extension   string   `json:"extension"`
	}{
		FileID:      fileID,
		PeerAddr:    peerAddr,
		Name:        "large-file", // You might want to get the actual filename
		Size:        0,            // You should get the actual file size
		Description: "File shared via ForeverStore",
		Categories:  []string{"misc"},
		Extension:   filepath.Ext("large-file"),
	}

	// Convert to JSON
	jsonData, err := json.Marshal(announcement)
	if err != nil {
		return fmt.Errorf("failed to marshal announcement: %v", err)
	}

	// Create POST request
	url := fmt.Sprintf("%s/announce", trackerAddr)
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// Send request
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to announce to tracker, status code: %d, body: %s", resp.StatusCode, string(body))
	}

	log.Printf("Successfully announced to tracker at %s", trackerAddr)
	return nil
}
