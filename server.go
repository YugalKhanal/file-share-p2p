package main

import (
	"bytes"
	"context"
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
	activeFiles      map[string]*shared.Metadata // fileID -> metadata of actively shared files
	activeFilesMu    sync.RWMutex
	udpPeers         map[string]bool
	udpPeersMu       sync.RWMutex
}

// In server.go, update makeServer function
func makeServer(listenAddr, bootstrapNode string) *FileServer {
	tcpOpts := p2p.TCPTransportOpts{
		ListenAddr:    listenAddr,
		HandshakeFunc: p2p.NOPHandshakeFunc,
		Decoder:       p2p.DefaultDecoder{},
	}

	// Create both TCP and UDP transports
	tcpTransport := p2p.NewTCPTransport(tcpOpts)
	udpTransport := p2p.NewUDPTransport(listenAddr)

	// Create combined transport manager
	transportManager := NewTransportManager(tcpTransport, udpTransport)

	opts := FileServerOpts{
		ListenAddr:        listenAddr,
		StorageRoot:       "shared_files",
		PathTransformFunc: DefaultPathTransformFunc,
		Transport:         transportManager,
		BootstrapNodes:    []string{bootstrapNode},
	}

	server := &FileServer{
		opts:          opts,
		store:         NewStore(StoreOpts{Root: opts.StorageRoot, PathTransformFunc: opts.PathTransformFunc}),
		quitch:        make(chan struct{}),
		peers:         make(map[string]p2p.Peer),
		activeFiles:   make(map[string]*shared.Metadata),
		activeFilesMu: sync.RWMutex{},
		udpPeers:      make(map[string]bool),
	}

	// Set callbacks for both transports
	tcpTransport.SetOnPeer(server.onPeer)
	udpTransport.SetOnPeer(server.onPeer)
	udpTransport.OnUDPPeer = server.onUDPPeer
	udpTransport.OnUDPDataRequest = server.handleUDPDataRequest

	return server
}

func (s *FileServer) handleUDPDataRequest(fileID string, chunkIndex int) ([]byte, error) {
	log.Printf("FileServer.handleUDPDataRequest called for file %s, chunk %d", fileID, chunkIndex)

	// First check if we're actively seeding this file
	s.activeFilesMu.RLock()
	meta, isActive := s.activeFiles[fileID]
	s.activeFilesMu.RUnlock()

	if !isActive {
		return nil, fmt.Errorf("file %s is not being actively seeded", fileID)
	}

	log.Printf("File is actively being seeded, metadata: %+v", meta)

	if chunkIndex >= meta.NumChunks {
		return nil, fmt.Errorf("invalid chunk index %d, file only has %d chunks",
			chunkIndex, meta.NumChunks)
	}

	// Read chunk with retry logic
	var chunkData []byte
	var err error
	for retries := range 3 {
		log.Printf("Reading chunk %d from file %s (attempt %d/3)",
			chunkIndex, meta.OriginalPath, retries+1)

		chunkData, err = s.store.ReadChunk(meta.OriginalPath, chunkIndex)
		if err == nil {
			log.Printf("Successfully read chunk %d: %d bytes", chunkIndex, len(chunkData))
			break
		}
		log.Printf("Error reading chunk (attempt %d/3): %v", retries+1, err)
		time.Sleep(time.Duration(retries+1) * 100 * time.Millisecond)
	}

	if err != nil {
		return nil, fmt.Errorf("failed to read chunk after retries: %v", err)
	}

	log.Printf("Successfully read chunk %d (%d bytes) for UDP transfer",
		chunkIndex, len(chunkData))
	return chunkData, nil
}

func isPrivateIP(ip net.IP) bool {
	privateCIDRs := []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
	}
	for _, cidr := range privateCIDRs {
		_, subnet, _ := net.ParseCIDR(cidr)
		if subnet.Contains(ip) {
			return true
		}
	}
	return ip.IsLoopback()
}

func filterPeerList(peerList []string) []string {
	var filtered []string
	for _, peer := range peerList {
		addresses := strings.Split(peer, "|")
		for _, addr := range addresses {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				continue
			}
			ip := net.ParseIP(host)
			if ip != nil && !isPrivateIP(ip) {
				filtered = append(filtered, addr)
			}
		}
	}
	if len(filtered) == 0 {
		log.Printf("Warning: No public IP peers found, falling back to all peers")
		return peerList // fallback to all peers
	}
	return filtered
}

// Determines the peer's full address, combining the detected IP with the listening port
func getPeerAddress(listenAddr string) (string, error) {
	// Get both public and local IPs
	publicIP, err := shared.GetPublicIP()
	if err != nil {
		return "", fmt.Errorf("failed to get public IP: %v", err)
	}

	localIP, err := shared.GetLocalIP()
	if err != nil {
		return "", fmt.Errorf("failed to get local IP: %v", err)
	}

	// Extract port from listen address
	port := listenAddr
	if strings.HasPrefix(port, ":") {
		port = port[1:]
	} else {
		_, portStr, err := net.SplitHostPort(listenAddr)
		if err != nil {
			return "", fmt.Errorf("failed to extract port from listen address: %v", err)
		}
		port = portStr
	}

	// Return both addresses in a format that peers can use
	return fmt.Sprintf("%s:%s|%s:%s", publicIP, port, localIP, port), nil
}

func (s *FileServer) refreshPeers(fileID string) error {
	return s.connectToAllPeers(fileID)
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

func (s *FileServer) downloadChunk(fileID string, chunkIndex, chunkSize int, outputFile *os.File) error {
	// At the beginning of the method
	log.Printf("Starting download for chunk %d of file %s", chunkIndex, fileID)

	// Track retry attempts per piece
	maxRetries := 3
	var lastErr error

	// Try UDP peers first
	s.udpPeersMu.RLock()
	hasUDPPeers := len(s.udpPeers) > 0
	log.Printf("Checking UDP peers for download: found %d peers", len(s.udpPeers))
	udpPeers := make([]string, 0, len(s.udpPeers))
	for peer := range s.udpPeers {
		udpPeers = append(udpPeers, peer)
		log.Printf("Available UDP peer: %s", peer)
	}
	s.udpPeersMu.RUnlock()

	if hasUDPPeers {
		// Create a channel for receiving the UDP response
		respChan := make(chan []byte, 1)
		errChan := make(chan error, 1)

		// Register a handler for UDP responses
		transport, ok := s.opts.Transport.(*p2p.TCPTransport)
		if ok {
			log.Printf("Got TCPTransport for UDP data request")
			// Use a unique request ID to track this specific request
			requestID := fmt.Sprintf("%s-%d-%d", fileID, chunkIndex, time.Now().UnixNano())
			log.Printf("Generated request ID: %s", requestID)

			// Setup a response handler with better logging
			transport.RegisterUDPResponseHandler(requestID, func(data []byte, err error) {
				log.Printf("UDP response handler called for request %s", requestID)
				if err != nil {
					log.Printf("UDP download error: %v", err)
					select {
					case errChan <- err:
						log.Printf("Sent error to error channel")
					default:
						log.Printf("Error channel full, couldn't send error")
					}
					return
				}

				log.Printf("Received UDP chunk data response: %d bytes", len(data))
				select {
				case respChan <- data:
					log.Printf("Sent data to response channel")
				default:
					log.Printf("Warning: Response channel is full")
				}
			})

			// Clean up when done
			defer transport.UnregisterUDPResponseHandler(requestID)

			// Try each UDP peer until success or all fail
			for _, udpPeer := range udpPeers {
				log.Printf("Requesting chunk %d via UDP from peer %s", chunkIndex, udpPeer)

				// Send the request with better logging
				err := transport.RequestUDPData(udpPeer, fileID, chunkIndex, requestID)
				if err != nil {
					log.Printf("Failed to send UDP request to %s: %v", udpPeer, err)
					continue
				}

				log.Printf("Successfully sent UDP request to %s", udpPeer)

				// Wait for response with timeout
				timeout := 10 * time.Second
				select {
				case data := <-respChan:
					log.Printf("Received UDP data response: %d bytes", len(data))

					// Write data to file
					if err := writeAndVerifyChunk(data, outputFile, chunkIndex, chunkSize); err != nil {
						log.Printf("Failed to write UDP chunk data: %v", err)
						continue
					}

					log.Printf("Successfully downloaded chunk %d via UDP", chunkIndex)
					return nil

				case err := <-errChan:
					log.Printf("UDP download error: %v", err)
					continue

				case <-time.After(timeout):
					log.Printf("UDP request timed out for peer %s", udpPeer)
					continue
				}
			}

			log.Printf("All UDP peers failed, falling back to TCP")
		} else {
			log.Printf("Error: Could not get TCPTransport for UDP data request")
		}
	}

	// Fall back to TCP peers if UDP failed or not available
	for attempt := range maxRetries {
		// Get current list of peers and shuffle them for load balancing
		s.mu.RLock()
		peers := make([]p2p.Peer, 0, len(s.peers))
		for _, peer := range s.peers {
			peers = append(peers, peer)
		}
		s.mu.RUnlock()

		if len(peers) == 0 {
			// Try to refresh peers if none available
			if err := s.refreshPeers(fileID); err != nil {
				time.Sleep(time.Second * time.Duration(attempt+1))
				continue
			}
			s.mu.RLock()
			for _, peer := range s.peers {
				peers = append(peers, peer)
			}
			s.mu.RUnlock()
			if len(peers) == 0 {
				continue
			}
		}

		// Randomize peer order for better load distribution
		rand.Shuffle(len(peers), func(i, j int) {
			peers[i], peers[j] = peers[j], peers[i]
		})

		// Try each peer for this attempt
		for _, peer := range peers {
			responseChan := make(chan p2p.MessageChunkResponse, 1)
			errChan := make(chan error, 1)
			done := make(chan struct{})

			// Set up response handler
			s.mu.Lock()
			s.responseHandlers = append(s.responseHandlers, func(msg p2p.Message) {
				select {
				case <-done:
					return
				default:
				}

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
					select {
					case errChan <- fmt.Errorf("invalid payload type: %T", msg.Payload):
					default:
					}
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
				close(done)
				continue
			}

			if err := peer.Send(buf.Bytes()); err != nil {
				log.Printf("Failed to send request to peer %s: %v", peer.RemoteAddr(), err)
				close(done)
				continue
			}

			// Wait for response with adaptive timeout
			timeout := 30 * time.Second
			if chunkSize > 16*1024*1024 {
				timeout = time.Duration(chunkSize/(2*1024*1024)) * time.Second
			}

			// Add exponential backoff
			timeout += time.Duration(attempt*5) * time.Second

			select {
			case resp := <-responseChan:
				if err := writeAndVerifyChunk(resp.Data, outputFile, chunkIndex, chunkSize); err != nil {
					lastErr = fmt.Errorf("chunk write failed from peer %s: %v", peer.RemoteAddr(), err)
					close(done)
					continue
				}
				close(done)
				return nil

			case err := <-errChan:
				lastErr = fmt.Errorf("download failed from peer %s: %v", peer.RemoteAddr(), err)
				close(done)

				// Remove failed peer
				s.mu.Lock()
				delete(s.peers, peer.RemoteAddr().String())
				s.mu.Unlock()

				continue

			case <-time.After(timeout):
				lastErr = fmt.Errorf("timeout waiting for peer %s", peer.RemoteAddr())
				close(done)
				continue
			}
		}

		// If we got here, all peers failed for this attempt
		if attempt < maxRetries-1 {
			log.Printf("All peers failed for piece %d, attempt %d/%d. Last error: %v",
				chunkIndex, attempt+1, maxRetries, lastErr)
			time.Sleep(time.Second * time.Duration(attempt+1))
			continue
		}
	}

	return fmt.Errorf("all peers failed after %d attempts. last error: %v", maxRetries, lastErr)
}

func writeAndVerifyChunk(data []byte, output *os.File, chunkIndex, chunkSize int) error {
	startOffset := int64(chunkIndex * chunkSize)

	// Verify chunk size
	if len(data) > chunkSize {
		return fmt.Errorf("oversized chunk data")
	}

	// Write chunk with retry
	for retries := range 3 {
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

// Handle RPC messages
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

func (s *FileServer) connectToAllPeers(fileID string) error {
	url := fmt.Sprintf("%s/peers?file_id=%s", s.opts.TrackerAddr, fileID)
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("failed to get peers from tracker: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("tracker returned error: %s", string(body))
	}

	var peerList []string
	if err := json.NewDecoder(resp.Body).Decode(&peerList); err != nil {
		return fmt.Errorf("failed to decode peer list: %v", err)
	}

	if len(peerList) == 0 {
		return fmt.Errorf("no peers are currently sharing file %s", fileID)
	}

	// Filter peer list to avoid connecting to private IPs when not needed
	peerList = filterPeerList(peerList)

	log.Printf("Received %d peers from tracker", len(peerList))

	// Create a wait group to track connection attempts
	var wg sync.WaitGroup
	connectionsMade := 0
	var connectionsMutex sync.Mutex

	// Get own address to avoid connecting to self
	myAddr := s.opts.ListenAddr
	if strings.HasPrefix(myAddr, ":") {
		myAddr = "localhost" + myAddr
	}

	// Create a context with timeout for all connection attempts
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Add a channel for detecting when first successful connection is made
	firstSuccessCh := make(chan struct{}, 1)
	allDoneCh := make(chan struct{})

	// Connect to each peer in parallel
	for _, peerAddr := range peerList {
		addresses := strings.Split(peerAddr, "|")
		for _, addr := range addresses {
			if addr == myAddr {
				log.Printf("Skipping own address: %s", addr)
				continue
			}

			// Check if we're already connected to this peer
			s.mu.RLock()
			_, alreadyConnected := s.peers[addr]
			s.mu.RUnlock()

			if alreadyConnected {
				continue
			}

			wg.Add(1)
			go func(addr string) {
				defer wg.Done()

				log.Printf("Attempting to connect to peer: %s", addr)

				// Try to establish connection
				connErr := s.opts.Transport.Dial(addr)

				// Check if connection succeeded
				select {
				case <-ctx.Done():
					// Context already canceled, no need to continue
					return
				default:
					if connErr != nil {
						log.Printf("Failed to connect to peer %s: %v", addr, connErr)
					} else {
						// Wait briefly for the connection to be established
						time.Sleep(500 * time.Millisecond)

						s.mu.RLock()
						_, connected := s.peers[addr]
						s.mu.RUnlock()

						if connected {
							log.Printf("Successfully connected to peer: %s", addr)
							connectionsMutex.Lock()
							connectionsMade++
							connectionsMutex.Unlock()

							// Signal that we have at least one connection
							select {
							case firstSuccessCh <- struct{}{}:
							default:
							}
						}
					}
				}
			}(addr)
		}
	}

	// Start a goroutine to wait for all connection attempts
	go func() {
		wg.Wait()
		close(allDoneCh)
	}()

	// Wait for either first success, all attempts to finish, or timeout
	select {
	case <-firstSuccessCh:
		// Got at least one connection, we can proceed
		log.Printf("Successfully established at least one peer connection")
		return nil
	case <-allDoneCh:
		// All connection attempts finished, check results
		connectionsMutex.Lock()
		numConnections := connectionsMade
		connectionsMutex.Unlock()

		if numConnections == 0 {
			return fmt.Errorf("failed to establish any peer connections")
		}
		log.Printf("Connected to %d peers for file %s", numConnections, fileID)
		return nil
	case <-ctx.Done():
		return fmt.Errorf("timed out waiting for peer connections")
	}
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

// Update the onUDPPeer function in server.go
func (s *FileServer) onUDPPeer(peerAddr string) {
	log.Printf("New UDP peer available: %s", peerAddr)

	// Store the UDP peer
	s.udpPeersMu.Lock()
	if s.udpPeers == nil {
		s.udpPeers = make(map[string]bool)
	}
	s.udpPeers[peerAddr] = true
	s.udpPeersMu.Unlock()

	log.Printf("Added UDP peer %s to available peer list (total UDP peers: %d)",
		peerAddr, len(s.udpPeers))
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

	// Add file to active files
	s.activeFilesMu.Lock()
	s.activeFiles[fileID] = meta
	s.activeFilesMu.Unlock()

	// Try to announce to tracker if configured
	if s.opts.TrackerAddr != "" {
		// Initial announcement
		if err := announceToTracker(s.opts.TrackerAddr, fileID, s.opts.ListenAddr, meta); err != nil {
			log.Printf("Warning: Failed to announce to tracker: %v", err)
			log.Printf("File will only be available for local sharing until tracker connection is restored")
		}

		// Start periodic announcements
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()

			for {
				select {
				case <-ticker.C:
					s.activeFilesMu.RLock()
					activeMeta, isActive := s.activeFiles[fileID]
					s.activeFilesMu.RUnlock()

					if !isActive {
						log.Printf("File %s is no longer active, stopping announcements", fileID)
						return
					}

					if err := announceToTracker(s.opts.TrackerAddr, fileID, s.opts.ListenAddr, activeMeta); err != nil {
						log.Printf("Failed to re-announce file %s: %v", fileID, err)
					} else {
						log.Printf("Successfully re-announced file %s to tracker", fileID)
					}
				case <-s.quitch:
					log.Printf("Stopping announcements for file %s due to server shutdown", fileID)
					return
				}
			}
		}()
	}

	log.Printf("File %s shared with ID %s and %d chunks", filePath, fileID, len(chunkHashes))
	return nil
}

func (s *FileServer) StopSeedingFile(fileID string) error {
	s.activeFilesMu.Lock()
	delete(s.activeFiles, fileID)
	s.activeFilesMu.Unlock()

	// Immediately notify tracker
	if s.opts.TrackerAddr == "" {
		return fmt.Errorf("no tracker address configured")
	}

	peerAddr, err := getPeerAddress(s.opts.ListenAddr)
	if err != nil {
		return fmt.Errorf("failed to get peer address: %v", err)
	}

	// Send removal notification to tracker
	url := fmt.Sprintf("%s/remove", s.opts.TrackerAddr)
	removal := struct {
		FileID   string `json:"file_id"`
		PeerAddr string `json:"peer_addr"`
	}{
		FileID:   fileID,
		PeerAddr: peerAddr,
	}

	jsonData, err := json.Marshal(removal)
	if err != nil {
		return fmt.Errorf("failed to marshal removal data: %v", err)
	}

	resp, err := http.Post(url, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to notify tracker: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("tracker returned error status: %d", resp.StatusCode)
	}

	return nil
}

func (s *FileServer) Cleanup() {
	log.Printf("Starting cleanup...")

	// Get list of active files
	s.activeFilesMu.RLock()
	activeFiles := make([]string, 0, len(s.activeFiles))
	for fileID := range s.activeFiles {
		activeFiles = append(activeFiles, fileID)
	}
	s.activeFilesMu.RUnlock()

	// Stop seeding each file
	for _, fileID := range activeFiles {
		s.StopSeedingFile(fileID)
	}

	// Close the quit channel
	close(s.quitch)
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

func (s *FileServer) DownloadFile(fileID string) error {
	log.Printf("Attempting to download file with ID: %s", fileID)

	// First check if the file exists and is being shared
	url := fmt.Sprintf("%s/metadata?file_id=%s", s.opts.TrackerAddr, fileID)
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("failed to connect to tracker: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("file %s is not available for download", fileID)
	}

	var fileInfo p2p.FileInfo
	if err := json.NewDecoder(resp.Body).Decode(&fileInfo); err != nil {
		return fmt.Errorf("failed to decode file info: %v", err)
	}

	if fileInfo.NumPeers == 0 {
		return fmt.Errorf("file %s exists but no peers are currently sharing it", fileID)
	}

	// Convert FileInfo to Metadata
	meta := &shared.Metadata{
		FileID:        fileInfo.FileID,
		NumChunks:     fileInfo.NumChunks,
		ChunkSize:     fileInfo.ChunkSize,
		FileExtension: fileInfo.Extension,
		ChunkHashes:   fileInfo.ChunkHashes,
		TotalHash:     fileInfo.TotalHash,
		TotalSize:     fileInfo.TotalSize,
	}

	// Save metadata locally
	if err := s.store.saveMetadata(meta); err != nil {
		return fmt.Errorf("failed to save metadata locally: %v", err)
	}

	// Initialize UDP peers map if not already done
	if s.udpPeers == nil {
		s.udpPeers = make(map[string]bool)
	}

	// Connect to all available peers, not just one
	if err := s.connectToAllPeers(fileID); err != nil {
		log.Printf("Warning: Could not connect to all peers: %v", err)
	}

	// Wait a moment for UDP connections to be fully established
	time.Sleep(1 * time.Second)

	// Log UDP peer counts
	s.udpPeersMu.RLock()
	log.Printf("Available UDP peers for download: %d", len(s.udpPeers))
	for peer := range s.udpPeers {
		log.Printf("  - UDP peer: %s", peer)
	}
	s.udpPeersMu.RUnlock()

	// Initialize piece manager
	pieceManager := NewPieceManager(meta.NumChunks, meta.ChunkHashes)

	// Initialize piece availability for each peer
	s.mu.RLock()
	for addr := range s.peers {
		pieces := make([]int, meta.NumChunks)
		for i := range meta.NumChunks {
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
	defer func() {
		outputFile.Close()
		// If download failed, remove the empty file
		if fileInfo, err := os.Stat(outputFileName); err == nil && fileInfo.Size() == 0 {
			os.Remove(outputFileName)
		}
	}()

	// Create semaphore to limit concurrent downloads
	const maxConcurrent = 10
	sem := make(chan struct{}, maxConcurrent)

	// Create error channel with buffer for all pieces
	errorChan := make(chan error, meta.NumChunks)
	var wg sync.WaitGroup

	// Track completed pieces for progress reporting
	completedPieces := 0
	var progressMutex sync.Mutex

	// Record start time for progress calculations
	startTime := time.Now()

	// Progress reporting goroutine
	progressTicker := time.NewTicker(5 * time.Second)
	defer progressTicker.Stop()

	// Channel to signal worker goroutines to stop
	done := make(chan struct{})
	defer close(done)

	// Start periodic peer refresh in background
	go func() {
		refreshTicker := time.NewTicker(20 * time.Second)
		defer refreshTicker.Stop()

		for {
			select {
			case <-refreshTicker.C:
				if err := s.connectToAllPeers(fileID); err != nil {
					log.Printf("Failed to refresh peers: %v", err)
				}
			case <-done:
				return
			}
		}
	}()

	// Progress reporting goroutine
	go func() {
		for range progressTicker.C {
			progressMutex.Lock()
			currentCompleted := completedPieces
			progressMutex.Unlock()

			if currentCompleted >= meta.NumChunks {
				return
			}

			piecesPerSecond := float64(currentCompleted) / time.Since(startTime).Seconds()
			remainingPieces := meta.NumChunks - currentCompleted
			eta := float64(remainingPieces) / piecesPerSecond
			if piecesPerSecond == 0 {
				eta = float64(0)
			}

			// Also log the number of connected peers
			s.mu.RLock()
			numPeers := len(s.peers)
			s.mu.RUnlock()

			// Check UDP peers too
			s.udpPeersMu.RLock()
			numUDPPeers := len(s.udpPeers)
			s.udpPeersMu.RUnlock()

			log.Printf("Progress: %d/%d pieces (%.2f%%) - %.2f pieces/sec - ETA: %.1f seconds - Connected peers: %d (TCP), %d (UDP)",
				currentCompleted, meta.NumChunks,
				float64(currentCompleted)/float64(meta.NumChunks)*100,
				piecesPerSecond, eta, numPeers, numUDPPeers)
		}
	}()

	// Keep track of active download attempts
	activeDownloads := make(map[int]bool)
	var activeMutex sync.Mutex

	// Main download loop
	for !pieceManager.IsComplete() {
		// Get available peers (both TCP and UDP)
		s.mu.RLock()
		peerCount := len(s.peers)
		s.mu.RUnlock()

		s.udpPeersMu.RLock()
		udpPeerCount := len(s.udpPeers)
		s.udpPeersMu.RUnlock()

		totalPeerCount := peerCount + udpPeerCount

		if totalPeerCount == 0 {
			// No peers available, try to refresh peers
			if err := s.connectToAllPeers(fileID); err != nil {
				return fmt.Errorf("all peers disconnected and failed to find new peers")
			}
			time.Sleep(5 * time.Second)
			continue
		}

		// Request more pieces than we have workers to keep the pipeline full
		requestMultiple := 2
		if totalPeerCount > 1 {
			requestMultiple = totalPeerCount * 2
		}
		pieces := pieceManager.GetNextPieces(maxConcurrent * requestMultiple)

		if len(pieces) == 0 {
			// Check if we have any active downloads before breaking
			activeMutex.Lock()
			activeCount := len(activeDownloads)
			activeMutex.Unlock()

			if activeCount == 0 {
				break
			}
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

				// Download the chunk
				err := s.downloadChunk(fileID, p.Index, meta.ChunkSize, outputFile)
				if err == nil {
					pieceManager.MarkPieceStatus(p.Index, PieceVerified)
					progressMutex.Lock()
					completedPieces++
					progressMutex.Unlock()
					return
				}

				log.Printf("Failed to download piece %d: %v", p.Index, err)
				pieceManager.MarkPieceStatus(p.Index, PieceMissing)
				select {
				case errorChan <- fmt.Errorf("failed to download piece %d: %v", p.Index, err):
				default:
				}
			}(piece)
		}
	}

	// Wait for all downloads to complete
	wg.Wait()

	// Verify the download was successful
	if !pieceManager.IsComplete() {
		return fmt.Errorf("download incomplete - some pieces could not be retrieved")
	}

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
func announceToTracker(trackerAddr string, fileID string, listenAddr string, metadata *shared.Metadata) error {
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
		NumChunks   int      `json:"num_chunks"`
		ChunkSize   int      `json:"chunk_size"`
		ChunkHashes []string `json:"chunk_hashes"`
		TotalHash   string   `json:"total_hash"`
		TotalSize   int64    `json:"total_size"`
	}{
		FileID:      fileID,
		PeerAddr:    peerAddr,
		Name:        filepath.Base(metadata.OriginalPath),
		Size:        metadata.TotalSize,
		Description: "File shared via ForeverStore",
		Categories:  []string{"misc"},
		Extension:   metadata.FileExtension,
		NumChunks:   metadata.NumChunks,
		ChunkSize:   metadata.ChunkSize,
		ChunkHashes: metadata.ChunkHashes,
		TotalHash:   metadata.TotalHash,
		TotalSize:   metadata.TotalSize,
	}

	// Convert to JSON
	jsonData, err := json.Marshal(announcement)
	if err != nil {
		return fmt.Errorf("failed to marshal announcement: %v", err)
	}

	// Retry configuration
	maxRetries := 3
	backoff := time.Second

	var lastErr error
	for attempt := range maxRetries {
		if attempt > 0 {
			log.Printf("Retrying tracker announcement (attempt %d/%d) after %v",
				attempt+1, maxRetries, backoff)
			time.Sleep(backoff)
			backoff *= 2 // Exponential backoff
		}

		// Create POST request
		url := fmt.Sprintf("%s/announce", trackerAddr)
		req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
		if err != nil {
			lastErr = fmt.Errorf("failed to create request: %v", err)
			continue
		}
		req.Header.Set("Content-Type", "application/json")

		// Create client with timeout
		client := &http.Client{
			Timeout: 5 * time.Second,
		}

		// Send request
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			log.Printf("Announcement attempt failed: %v", err)
			continue
		}

		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			lastErr = fmt.Errorf("tracker returned status %d: %s",
				resp.StatusCode, string(body))
			continue
		}

		// Success!
		log.Printf("Successfully announced to tracker at %s", trackerAddr)
		return nil
	}

	return fmt.Errorf("failed to announce to tracker after %d attempts: %v",
		maxRetries, lastErr)
}
