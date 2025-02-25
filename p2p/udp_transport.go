package p2p

import (
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anthdm/foreverstore/shared"
)

// UDPTransport handles communication between peers using UDP for hole punching
// and data transfer
type UDPTransport struct {
	ListenAddr          string
	udpConn             *net.UDPConn
	rpcch               chan RPC
	connectedCh         chan string
	udpPeers            sync.Map // Map of addr string -> *net.UDPAddr
	udpDataCh           chan UDPDataPacket
	udpResponseHandlers sync.Map // Map of requestID -> handler function
	udpRequestsMu       sync.Mutex
	udpRequests         map[string]UDPRequest // Track active requests by requestID
	OnPeer              func(Peer) error
	OnUDPPeer           func(string)
	OnUDPDataRequest    func(string, int) ([]byte, error)
}

// NewUDPTransport creates a new UDPTransport
func NewUDPTransport(listenAddr string) *UDPTransport {
	// Use the provided address directly, no offset
	t := &UDPTransport{
		ListenAddr:  listenAddr,
		rpcch:       make(chan RPC, 1024),
		connectedCh: make(chan string, 10),
		udpDataCh:   make(chan UDPDataPacket, 100),
		udpRequests: make(map[string]UDPRequest),
	}

	// Log the UDP port
	log.Printf("UDP transport using address %s", listenAddr)

	// Start the UDP request timeout checker
	go t.StartUDPRequestTimeoutChecker()

	return t
}

// Addr implements the Transport interface return the address
// the transport is accepting connections.
func (t *UDPTransport) Addr() string {
	return t.ListenAddr
}

// Consume implements the Transport interface, which will return read-only channel
// for reading the incoming messages received from another peer in the network.
func (t *UDPTransport) Consume() <-chan RPC {
	return t.rpcch
}

// Close implements the Transport interface.
func (t *UDPTransport) Close() error {
	if t.udpConn != nil {
		return t.udpConn.Close()
	}
	return nil
}

// SetOnPeer sets the callback for when a new peer connects
func (t *UDPTransport) SetOnPeer(handler func(Peer) error) {
	t.OnPeer = handler
}

// Dial implements the Transport interface for UDP connections.
func (t *UDPTransport) Dial(addr string) error {
	// Initialize UDP listener if not already done
	if t.udpConn == nil {
		log.Printf("Initializing UDP listener for hole punching")
		if err := t.setupUDPListener(); err != nil {
			return fmt.Errorf("UDP setup failed: %v", err)
		}
	}

	addrs := strings.Split(addr, "|")
	log.Printf("Attempting UDP hole punching to addresses: %v", addrs)

	// Create a wait group for the punch attempts
	var wg sync.WaitGroup

	// Try hole punching for each address - attempt with ALL addresses, not just odd ones
	// as the peer might have announced in a different format
	for _, address := range addrs {
		wg.Add(1)
		go func(targetAddr string) {
			defer wg.Done()

			// Skip invalid addresses
			host, portStr, err := net.SplitHostPort(targetAddr)
			if err != nil {
				log.Printf("Invalid address %s: %v", targetAddr, err)
				return
			}

			port, err := strconv.Atoi(portStr)
			if err != nil {
				log.Printf("Invalid port in address %s: %v", targetAddr, err)
				return
			}

			// Create UDP address
			udpAddr := &net.UDPAddr{
				IP:   net.ParseIP(host),
				Port: port,
			}

			// Get our listen port
			listenPort := t.getPort()

			// Create a PUNCH message with the public IP
			publicIP, err := shared.GetPublicIP()
			if err != nil {
				log.Printf("Warning: Failed to get public IP: %v", err)
				// Fallback to local IP if public can't be determined
				publicIP, err = shared.GetLocalIP()
				if err != nil {
					log.Printf("Cannot determine IP address: %v", err)
					return
				}
			}

			// Create and send the PUNCH message
			punchMsg := fmt.Sprintf("PUNCH:%s:%d", publicIP, listenPort)
			log.Printf("Starting hole punching to %s (UDP: %s) with message: %s",
				targetAddr, udpAddr, punchMsg)

			// Send punch messages with exponential backoff
			for i := range 5 {
				n, err := t.udpConn.WriteToUDP([]byte(punchMsg), udpAddr)
				if err != nil {
					log.Printf("Failed to send punch message to %s: %v", udpAddr, err)
				} else {
					log.Printf("Sent punch message to %s (%d bytes)", udpAddr, n)
				}

				// Exponential backoff: 100ms, 200ms, 400ms, 800ms, 1600ms
				backoff := time.Duration(100*(1<<uint(i))) * time.Millisecond
				time.Sleep(backoff)
			}
		}(address)
	}

	// Start a goroutine to wait for all punch attempts
	go func() {
		wg.Wait()
		log.Printf("All hole punching attempts completed")
	}()

	// Wait for connection or timeout
	select {
	case peerAddr := <-t.connectedCh:
		log.Printf("Connection established via UDP hole punching to %s", peerAddr)

		// Store the peer address for future communication
		host, portStr, err := net.SplitHostPort(peerAddr)
		if err != nil {
			log.Printf("Invalid peer address from ACK: %s", peerAddr)
			return fmt.Errorf("invalid address from ACK: %s", peerAddr)
		}

		port, err := strconv.Atoi(portStr)
		if err != nil {
			log.Printf("Invalid port in peer address: %s", peerAddr)
			return fmt.Errorf("invalid port in peer address: %s", peerAddr)
		}

		// Create UDP address and store it
		udpPeerAddr := &net.UDPAddr{
			IP:   net.ParseIP(host),
			Port: port,
		}

		t.udpPeers.Store(peerAddr, udpPeerAddr)

		// Notify that UDP connection is available
		if t.OnUDPPeer != nil {
			t.OnUDPPeer(peerAddr)
		}

		log.Printf("Notified FileServer about new UDP peer: %s", peerAddr)

		return nil
	case <-time.After(15 * time.Second):
		log.Printf("UDP hole punching timed out")
		return fmt.Errorf("UDP hole punching timeout")
	}
}

// ListenAndAccept starts the UDP listener
func (t *UDPTransport) ListenAndAccept() error {
	return t.setupUDPListener()
}

func (t *UDPTransport) getPort() int {
	parts := strings.Split(t.ListenAddr, ":")
	if len(parts) != 2 {
		return 0
	}
	port, _ := strconv.Atoi(parts[1])
	return port
}

// RequestUDPData sends a data request over UDP
func (t *UDPTransport) RequestUDPData(peerAddr, fileID string, chunkIndex int, requestID string) error {
	// Get the UDPAddr for this peer
	addrObj, ok := t.udpPeers.Load(peerAddr)
	if !ok {
		log.Printf("Warning: UDP peer %s not found in peer map", peerAddr)
		// Try to extract address and port
		host, portStr, err := net.SplitHostPort(peerAddr)
		if err != nil {
			return fmt.Errorf("invalid peer address format: %s", peerAddr)
		}

		port, err := strconv.Atoi(portStr)
		if err != nil {
			return fmt.Errorf("invalid port in peer address: %s", peerAddr)
		}

		// Create UDP address
		udpAddr := &net.UDPAddr{
			IP:   net.ParseIP(host),
			Port: port,
		}

		// Store for future use
		t.udpPeers.Store(peerAddr, udpAddr)
		addrObj = udpAddr
		log.Printf("Created new UDP address for peer: %s", udpAddr)
	}

	udpAddr, ok := addrObj.(*net.UDPAddr)
	if !ok {
		return fmt.Errorf("invalid UDP peer address type for %s", peerAddr)
	}

	// Create request message
	// Format: DATA_REQ:<requestID>:<fileID>:<chunkIndex>
	reqMsg := fmt.Sprintf("DATA_REQ:%s:%s:%d", requestID, fileID, chunkIndex)

	// Send the request
	n, err := t.udpConn.WriteToUDP([]byte(reqMsg), udpAddr)
	if err != nil {
		return fmt.Errorf("failed to send UDP data request: %v", err)
	}

	log.Printf("Sent UDP data request for file %s chunk %d to %s (requestID: %s, bytes: %d)",
		fileID, chunkIndex, udpAddr, requestID, n)

	// Track this request
	t.udpRequestsMu.Lock()
	t.udpRequests[requestID] = UDPRequest{
		FileID:     fileID,
		ChunkIndex: chunkIndex,
		PeerAddr:   udpAddr,
		Timestamp:  time.Now(),
		Attempts:   1,
	}
	t.udpRequestsMu.Unlock()

	return nil
}

// RegisterUDPResponseHandler registers a handler for UDP responses
func (t *UDPTransport) RegisterUDPResponseHandler(requestID string, handler func([]byte, error)) {
	t.udpResponseHandlers.Store(requestID, handler)
}

// UnregisterUDPResponseHandler removes a response handler
func (t *UDPTransport) UnregisterUDPResponseHandler(requestID string) {
	t.udpResponseHandlers.Delete(requestID)

	// Also clean up the request
	t.udpRequestsMu.Lock()
	delete(t.udpRequests, requestID)
	t.udpRequestsMu.Unlock()
}

// StartUDPRequestTimeoutChecker monitors and retries UDP requests
func (t *UDPTransport) StartUDPRequestTimeoutChecker() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()

		// Check for timed out requests
		t.udpRequestsMu.Lock()
		for id, req := range t.udpRequests {
			// If request is older than 10 seconds
			if now.Sub(req.Timestamp) > 10*time.Second {
				// Max retries reached?
				if req.Attempts >= 3 {
					// Call the handler with an error
					handlerObj, exists := t.udpResponseHandlers.Load(id)
					if exists {
						if handler, ok := handlerObj.(func([]byte, error)); ok {
							handler(nil, fmt.Errorf("UDP request timed out after %d attempts", req.Attempts))
						}
					}

					// Clean up
					delete(t.udpRequests, id)
					t.udpResponseHandlers.Delete(id)
				} else {
					// Retry the request
					req.Attempts++
					req.Timestamp = now
					t.udpRequests[id] = req

					// Send the request again
					reqMsg := fmt.Sprintf("DATA_REQ:%s:%s:%d", id, req.FileID, req.ChunkIndex)
					t.udpConn.WriteToUDP([]byte(reqMsg), req.PeerAddr)

					log.Printf("Retrying UDP request for file %s chunk %d (attempt %d/3)",
						req.FileID, req.ChunkIndex, req.Attempts)
				}
			}
		}
		t.udpRequestsMu.Unlock()
	}
}

// setupUDPListener initializes and starts the UDP listener
func (t *UDPTransport) setupUDPListener() error {
	log.Printf("Setting up UDP listener on %s", t.ListenAddr)

	// Parse the UDP address
	addr, err := net.ResolveUDPAddr("udp", t.ListenAddr)
	if err != nil {
		return fmt.Errorf("failed to resolve UDP address: %v", err)
	}

	// Create the UDP connection
	t.udpConn, err = net.ListenUDP("udp", addr)
	if err != nil {
		return fmt.Errorf("UDP listen failed: %v", err)
	}

	log.Printf("UDP listener established successfully on %s", t.udpConn.LocalAddr())

	// Start handling incoming UDP messages
	go t.handleUDPMessages()
	return nil
}

// Setup UDP data channel for file transfer
func (t *UDPTransport) setupUDPDataChannel(remoteAddr *net.UDPAddr) {
	log.Printf("UDP data channel established with %s", remoteAddr)

	// Notify our FileServer that this peer is available via UDP
	// (This would require extending the FileServer to handle UDP peers)
	if t.OnUDPPeer != nil {
		t.OnUDPPeer(remoteAddr.String())
	}
}

// Handle UDP data request
func (t *UDPTransport) handleUDPDataRequest(remoteAddr *net.UDPAddr, fileID string, chunkIndex int, requestID string) {
	log.Printf("Received UDP data request for file %s chunk %d from %s (requestID: %s)",
		fileID, chunkIndex, remoteAddr, requestID)

	if t.OnUDPDataRequest != nil {
		chunkData, err := t.OnUDPDataRequest(fileID, chunkIndex)
		if err != nil {
			log.Printf("Failed to fetch chunk data: %v", err)

			// Send error response
			errMsg := fmt.Sprintf("DATA_ERR:%s:%s:%d:Error fetching chunk: %v",
				requestID, fileID, chunkIndex, err)
			t.udpConn.WriteToUDP([]byte(errMsg), remoteAddr)
			return
		}

		// Define maximum UDP packet size for data (leaving room for headers)
		maxUDPDataSize := 60 * 1024 // 60KB is a safe size for most networks

		// Check if we need to split the data
		if len(chunkData) > maxUDPDataSize {
			log.Printf("Chunk data size (%d bytes) exceeds safe UDP packet size, splitting into multiple packets", len(chunkData))

			// Calculate number of packets needed
			numPackets := (len(chunkData) + maxUDPDataSize - 1) / maxUDPDataSize

			// Send initial packet indicating a multi-packet response
			initMsg := fmt.Sprintf("DATA_MULTI:%s:%s:%d:%d:%d||",
				requestID, fileID, chunkIndex, len(chunkData), numPackets)

			_, err := t.udpConn.WriteToUDP([]byte(initMsg), remoteAddr)
			if err != nil {
				log.Printf("Failed to send multi-packet initialization: %v", err)
				return
			}

			// Send each packet with sequence number
			for i := range numPackets {
				start := i * maxUDPDataSize
				end := start + maxUDPDataSize
				if end > len(chunkData) {
					end = len(chunkData)
				}

				// Create header for this packet
				// Format: DATA_PART:<requestID>:<fileID>:<chunkIndex>:<packetNum>:<totalPackets>||
				headerStr := fmt.Sprintf("DATA_PART:%s:%s:%d:%d:%d||",
					requestID, fileID, chunkIndex, i, numPackets)

				// Create combined message
				packetData := make([]byte, len(headerStr)+end-start)
				copy(packetData, []byte(headerStr))
				copy(packetData[len(headerStr):], chunkData[start:end])

				// Send the packet
				_, err := t.udpConn.WriteToUDP(packetData, remoteAddr)
				if err != nil {
					log.Printf("Failed to send UDP data packet %d/%d: %v", i+1, numPackets, err)
					continue
				}

				log.Printf("Sent UDP data packet %d/%d (%d bytes) for chunk %d",
					i+1, numPackets, len(packetData), chunkIndex)

				// Small delay between packets to avoid overwhelming the network
				time.Sleep(time.Millisecond * 5)
			}

			log.Printf("Completed sending chunk %d in %d UDP packets", chunkIndex, numPackets)

		} else {
			// For small chunks, send in a single packet as before
			// Format: DATA_RESP:<requestID>:<fileID>:<chunkIndex>:<dataLength>||<actual data>
			headerStr := fmt.Sprintf("DATA_RESP:%s:%s:%d:%d||",
				requestID, fileID, chunkIndex, len(chunkData))

			// Create a combined message
			fullMsg := make([]byte, len(headerStr)+len(chunkData))
			copy(fullMsg, []byte(headerStr))
			copy(fullMsg[len(headerStr):], chunkData)

			// Send the combined message
			n, err := t.udpConn.WriteToUDP(fullMsg, remoteAddr)
			if err != nil {
				log.Printf("Failed to send UDP data response: %v", err)
			} else {
				log.Printf("Successfully sent chunk %d (%d bytes) via UDP", chunkIndex, n)
			}
		}
	} else {
		log.Printf("Cannot handle UDP data request: no data handler registered")

		// Send error response
		errMsg := fmt.Sprintf("DATA_ERR:%s:%s:%d:No data handler registered",
			requestID, fileID, chunkIndex)
		t.udpConn.WriteToUDP([]byte(errMsg), remoteAddr)
	}
}

func (t *UDPTransport) handleUDPMessages() {
	log.Printf("Starting UDP message handler")
	buf := make([]byte, 64*1024) // Large buffer for data chunks

	// Track multi-packet transfers
	multiPacketData := make(map[string][]byte)
	multiPacketInfo := make(map[string]struct {
		TotalSize     int
		NumPackets    int
		ReceivedParts int
		LastActivity  time.Time
	})
	var multiPacketMu sync.Mutex

	// Start a goroutine to clean up incomplete transfers
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		for range ticker.C {
			now := time.Now()
			multiPacketMu.Lock()
			for requestID, info := range multiPacketInfo {
				// Clean up transfers that haven't been active for 60 seconds
				if now.Sub(info.LastActivity) > 60*time.Second {
					delete(multiPacketInfo, requestID)
					delete(multiPacketData, requestID)
					log.Printf("Cleaned up stale multi-packet transfer: %s", requestID)
				}
			}
			multiPacketMu.Unlock()
		}
	}()

	for {
		n, remoteAddr, err := t.udpConn.ReadFromUDP(buf)
		if err != nil {
			log.Printf("UDP read error: %v", err)
			continue
		}

		// Make a copy of the data to prevent it from being overwritten
		data := make([]byte, n)
		copy(data, buf[:n])
		message := string(data)

		// For logging, truncate very long messages
		messagePreview := message
		if len(messagePreview) > 60 {
			messagePreview = messagePreview[:60] + "..." // Truncate for logging
		}
		log.Printf("Received UDP message from %s: %s", remoteAddr, messagePreview)

		// Handle different message types
		if strings.HasPrefix(message, "PUNCH:") {
			// Handle PUNCH messages
			parts := strings.SplitN(message, ":", 2)
			if len(parts) != 2 {
				log.Printf("Invalid PUNCH format from %s: %s", remoteAddr, message)
				continue
			}
			senderAddr := parts[1]
			log.Printf("Received PUNCH message from %s, sending ACK to %s",
				remoteAddr, senderAddr)

			// Send an ACK response
			ackMsg := fmt.Sprintf("ACK:%s", t.ListenAddr)
			_, err := t.udpConn.WriteToUDP([]byte(ackMsg), remoteAddr)
			if err != nil {
				log.Printf("Failed to send ACK: %v", err)
			} else {
				log.Printf("Sent ACK to %s", remoteAddr)

				// Store this peer for future UDP communication
				t.udpPeers.Store(remoteAddr.String(), remoteAddr)

				// Setup UDP data channel for this peer
				t.setupUDPDataChannel(remoteAddr)
			}

		} else if strings.HasPrefix(message, "ACK:") {
			// Handle ACK messages
			parts := strings.SplitN(message, ":", 2)
			if len(parts) != 2 {
				log.Printf("Invalid ACK format from %s: %s", remoteAddr, message)
				continue
			}
			senderAddr := parts[1]
			log.Printf("Received ACK from %s for hole punching", senderAddr)

			// Signal the Dial method that connection was successful
			select {
			case t.connectedCh <- remoteAddr.String():
				log.Printf("Signaled successful UDP connection to %s", remoteAddr)
			default:
				log.Printf("Connection channel full, couldn't signal")
			}

			// Store this peer for future UDP communication
			t.udpPeers.Store(remoteAddr.String(), remoteAddr)

		} else if strings.HasPrefix(message, "DATA_REQ:") {
			// Handle data request: DATA_REQ:<requestID>:<fileID>:<chunkIndex>
			parts := strings.Split(strings.TrimPrefix(message, "DATA_REQ:"), ":")
			if len(parts) != 3 {
				log.Printf("Invalid DATA_REQ format from %s: %s", remoteAddr, message)
				continue
			}

			requestID := parts[0]
			fileID := parts[1]
			chunkIndex, err := strconv.Atoi(parts[2])
			if err != nil {
				log.Printf("Invalid chunk index in DATA_REQ from %s: %s", remoteAddr, parts[2])
				continue
			}

			log.Printf("Received UDP data request for file %s chunk %d from %s (requestID: %s)",
				fileID, chunkIndex, remoteAddr, requestID)

			// Process request in a separate goroutine
			go t.handleUDPDataRequest(remoteAddr, fileID, chunkIndex, requestID)

		} else if strings.HasPrefix(message, "DATA_MULTI:") {
			// Format: DATA_MULTI:<requestID>:<fileID>:<chunkIndex>:<totalSize>:<numPackets>||
			delimIndex := strings.Index(message, "||")
			if delimIndex == -1 {
				log.Printf("Invalid DATA_MULTI format from %s: missing delimiter", remoteAddr)
				continue
			}

			// Parse header
			header := message[:delimIndex]
			parts := strings.Split(strings.TrimPrefix(header, "DATA_MULTI:"), ":")
			if len(parts) != 5 {
				log.Printf("Invalid DATA_MULTI header from %s: %s", remoteAddr, header)
				continue
			}

			requestID := parts[0]
			fileID := parts[1]
			chunkIndex, _ := strconv.Atoi(parts[2])
			totalSize, _ := strconv.Atoi(parts[3])
			numPackets, _ := strconv.Atoi(parts[4])

			log.Printf("Starting multi-packet transfer for file %s chunk %d (%d bytes in %d packets)",
				fileID, chunkIndex, totalSize, numPackets)

			// Initialize storage for this transfer
			multiPacketMu.Lock()
			multiPacketData[requestID] = make([]byte, totalSize)
			multiPacketInfo[requestID] = struct {
				TotalSize     int
				NumPackets    int
				ReceivedParts int
				LastActivity  time.Time
			}{
				TotalSize:     totalSize,
				NumPackets:    numPackets,
				ReceivedParts: 0,
				LastActivity:  time.Now(),
			}
			multiPacketMu.Unlock()

		} else if strings.HasPrefix(message, "DATA_PART:") {
			// Format: DATA_PART:<requestID>:<fileID>:<chunkIndex>:<packetNum>:<totalPackets>||<data>
			delimIndex := strings.Index(message, "||")
			if delimIndex == -1 {
				log.Printf("Invalid DATA_PART format from %s: missing delimiter", remoteAddr)
				continue
			}

			// Parse header
			header := message[:delimIndex]
			parts := strings.Split(strings.TrimPrefix(header, "DATA_PART:"), ":")
			if len(parts) != 5 {
				log.Printf("Invalid DATA_PART header from %s: %s", remoteAddr, header)
				continue
			}

			requestID := parts[0]
			fileID := parts[1]
			chunkIndex, _ := strconv.Atoi(parts[2])
			packetNum, _ := strconv.Atoi(parts[3])
			totalPackets, _ := strconv.Atoi(parts[4])

			// Get the data part (after the delimiter)
			packetData := data[delimIndex+2:]

			// Store this part
			multiPacketMu.Lock()
			info, exists := multiPacketInfo[requestID]
			if !exists {
				multiPacketMu.Unlock()
				log.Printf("Received DATA_PART for unknown request: %s", requestID)
				continue
			}

			// Calculate position in the buffer
			maxPartSize := 60 * 1024 // Same as in handleUDPDataRequest
			startPos := packetNum * maxPartSize
			endPos := startPos + len(packetData)
			if endPos > info.TotalSize {
				endPos = info.TotalSize
			}

			// Copy data to the correct position
			copy(multiPacketData[requestID][startPos:endPos], packetData)

			// Update received parts count and last activity
			info.ReceivedParts++
			info.LastActivity = time.Now()
			multiPacketInfo[requestID] = info

			log.Printf("Received part %d/%d for request %s (file %s chunk %d)",
				packetNum+1, totalPackets, requestID, fileID, chunkIndex)

			// Check if all parts received
			// In handleUDPMessages() function in udp_transport.go, update the multi-packet handling code:

			// When all parts of a multi-packet transfer are received:
			if info.ReceivedParts >= info.NumPackets {
				log.Printf("All %d parts received for request %s, processing complete response",
					info.NumPackets, requestID)

				// Reconstruct the full data
				fullData := multiPacketData[requestID]

				// Parse fileID and chunkIndex from the requestID (typically formatted as "fileID-chunkIndex-timestamp")
				parts := strings.Split(requestID, "-")
				if len(parts) >= 2 {
					fileID := parts[0]
					chunkIndex, _ := strconv.Atoi(parts[1])
					log.Printf("Successfully reconstructed chunk %d for file %s (%d bytes)",
						chunkIndex, fileID, len(fullData))
				}

				// Clean up
				delete(multiPacketData, requestID)
				delete(multiPacketInfo, requestID)
				multiPacketMu.Unlock()

				// Look up the handler for this response
				handlerObj, exists := t.udpResponseHandlers.Load(requestID)
				if !exists {
					log.Printf("No handler found for UDP response with ID %s", requestID)
					continue
				}

				// Call the handler with the data
				if handler, ok := handlerObj.(func([]byte, error)); ok {
					handler(fullData, nil)

					// Clean up after successful response
					t.udpResponseHandlers.Delete(requestID)
					t.udpRequestsMu.Lock()
					delete(t.udpRequests, requestID)
					t.udpRequestsMu.Unlock()

					log.Printf("Successfully processed multi-packet data response (%d bytes) for request %s",
						len(fullData), requestID)
				} else {
					log.Printf("Invalid handler type for UDP response with ID %s", requestID)
				}
			} else {
				multiPacketMu.Unlock()
			}

		} else if strings.HasPrefix(message, "DATA_RESP:") {
			// Find the delimiter between header and data
			delimIndex := strings.Index(message, "||")
			if delimIndex == -1 {
				log.Printf("Invalid DATA_RESP format from %s: missing delimiter", remoteAddr)
				continue
			}

			// Parse the header
			header := message[:delimIndex]
			parts := strings.Split(strings.TrimPrefix(header, "DATA_RESP:"), ":")
			if len(parts) != 4 {
				log.Printf("Invalid DATA_RESP header from %s: %s", remoteAddr, header)
				continue
			}

			requestID := parts[0]
			fileID := parts[1]
			chunkIndex, err := strconv.Atoi(parts[2])
			if err != nil {
				log.Printf("Invalid chunk index in DATA_RESP from %s: %s", remoteAddr, parts[2])
				continue
			}

			dataLength, err := strconv.Atoi(parts[3])
			if err != nil {
				log.Printf("Invalid data length in DATA_RESP from %s: %v, %s", remoteAddr, dataLength, parts[3])
				continue
			}

			// Extract the actual data part (after the delimiter)
			chunkData := data[delimIndex+2:]

			log.Printf("Received UDP data response for file %s chunk %d from %s (%d bytes)",
				fileID, chunkIndex, remoteAddr, len(chunkData))

			// Look up the handler for this response
			handlerObj, exists := t.udpResponseHandlers.Load(requestID)
			if !exists {
				log.Printf("No handler found for UDP response with ID %s", requestID)
				continue
			}

			// Call the handler with the data
			if handler, ok := handlerObj.(func([]byte, error)); ok {
				// Call the handler with the data
				handler(chunkData, nil)

				// Clean up after successful response
				t.udpResponseHandlers.Delete(requestID)
				t.udpRequestsMu.Lock()
				delete(t.udpRequests, requestID)
				t.udpRequestsMu.Unlock()

				log.Printf("Successfully processed data response (%d bytes) for request %s",
					len(chunkData), requestID)
			} else {
				log.Printf("Invalid handler type for UDP response with ID %s", requestID)
			}
		} else if strings.HasPrefix(message, "DATA_ERR:") {
			// Handle error response: DATA_ERR:<requestID>:<fileID>:<chunkIndex>:<error message>
			parts := strings.SplitN(strings.TrimPrefix(message, "DATA_ERR:"), ":", 4)
			if len(parts) != 4 {
				log.Printf("Invalid DATA_ERR format from %s: %s", remoteAddr, message)
				continue
			}

			requestID := parts[0]
			fileID := parts[1]
			errorMsg := parts[3]

			log.Printf("Received UDP error response for file %s from %s: %s",
				fileID, remoteAddr, errorMsg)

			// Look up the handler for this response
			handlerObj, exists := t.udpResponseHandlers.Load(requestID)
			if !exists {
				log.Printf("No handler found for UDP error with ID %s", requestID)
				continue
			}

			// Call the handler with the error
			if handler, ok := handlerObj.(func([]byte, error)); ok {
				handler(nil, fmt.Errorf("%s", errorMsg))

				// Clean up after error response
				t.udpResponseHandlers.Delete(requestID)
				t.udpRequestsMu.Lock()
				delete(t.udpRequests, requestID)
				t.udpRequestsMu.Unlock()
			} else {
				log.Printf("Invalid handler type for UDP error with ID %s", requestID)
			}
		} else {
			log.Printf("Received unknown UDP message type from %s: %s", remoteAddr, messagePreview)
		}
	}
}
