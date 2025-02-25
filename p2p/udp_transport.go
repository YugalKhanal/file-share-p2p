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
	// Parse the TCP address to get the port
	_, portStr, err := net.SplitHostPort(listenAddr)
	if err != nil {
		portStr = "3000" // Default port if parsing fails
	}

	// Use a UDP port that's offset from the TCP port
	port, _ := strconv.Atoi(portStr)
	udpPort := port + 4 // Change this to +4 to be consistent
	udpListenAddr := fmt.Sprintf(":%d", udpPort)

	t := &UDPTransport{
		ListenAddr:  udpListenAddr,
		rpcch:       make(chan RPC, 1024),
		connectedCh: make(chan string, 1),
		udpDataCh:   make(chan UDPDataPacket, 100),
		udpRequests: make(map[string]UDPRequest),
	}

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

	// Try hole punching for each address
	for i, address := range addrs {
		// Skip TCP addresses (even indexes in our format)
		if i%2 == 0 {
			continue // Skip TCP addresses when in UDP transport
		}

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
			return
		}

		// Send the data response
		dataResp := fmt.Sprintf("DATA_RESP:%s:%s:%d:", requestID, fileID, chunkIndex)

		// First send the header
		_, err = t.udpConn.WriteToUDP([]byte(dataResp), remoteAddr)
		if err != nil {
			log.Printf("Failed to send UDP data response header: %v", err)
			return
		}

		// Then send the data
		_, err = t.udpConn.WriteToUDP(chunkData, remoteAddr)
		if err != nil {
			log.Printf("Failed to send UDP data response: %v", err)
		} else {
			log.Printf("Successfully sent chunk %d (%d bytes) via UDP", chunkIndex, len(chunkData))
		}
	} else {
		log.Printf("Cannot handle UDP data request: no data request handler registered")
	}
}

func (t *UDPTransport) handleUDPMessages() {
	log.Printf("Starting UDP message handler")
	buf := make([]byte, 64*1024) // Large buffer for data chunks
	for {
		n, remoteAddr, err := t.udpConn.ReadFromUDP(buf)
		if err != nil {
			log.Printf("UDP read error: %v", err)
			continue
		}

		message := string(buf[:n])

		// Handle different message types
		if strings.HasPrefix(message, "PUNCH:") {
			// Extract the sender's address
			senderAddr := strings.TrimPrefix(message, "PUNCH:")
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
			// Extract the sender's address
			senderAddr := strings.TrimPrefix(message, "ACK:")
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

		} else if strings.HasPrefix(message, "DATA_RESP:") {
			// Handle data response: DATA_RESP:<requestID>:<fileID>:<chunkIndex>:<data>
			// Find the 4th colon (after the "DATA_RESP:" prefix)
			prefixLen := len("DATA_RESP:")
			rest := message[prefixLen:]

			// Find the position of the third colon in the rest of the string
			colonCount := 0
			headerEnd := 0
			for i, ch := range rest {
				if ch == ':' {
					colonCount++
					if colonCount == 3 {
						headerEnd = prefixLen + i + 1
						break
					}
				}
			}

			if headerEnd == 0 {
				log.Printf("Invalid DATA_RESP format from %s", remoteAddr)
				continue
			}

			header := message[:headerEnd-1]
			parts := strings.Split(header, ":")
			if len(parts) != 4 {
				log.Printf("Invalid DATA_RESP header from %s: %s", remoteAddr, header)
				continue
			}

			requestID := parts[1]
			fileID := parts[2]
			chunkIndex, err := strconv.Atoi(parts[3])
			if err != nil {
				log.Printf("Invalid chunk index in DATA_RESP from %s: %s", remoteAddr, parts[3])
				continue
			}

			// Extract the data part (everything after the header)
			data := buf[headerEnd:n]

			log.Printf("Received UDP data response for file %s chunk %d from %s (%d bytes)",
				fileID, chunkIndex, remoteAddr, len(data))

			// Look up the handler for this response
			handlerObj, exists := t.udpResponseHandlers.Load(requestID)
			if !exists {
				log.Printf("No handler found for UDP response with ID %s", requestID)
				continue
			}

			// Call the handler
			if handler, ok := handlerObj.(func([]byte, error)); ok {
				handler(data, nil)

				// Clean up after successful response
				t.udpResponseHandlers.Delete(requestID)
				t.udpRequestsMu.Lock()
				delete(t.udpRequests, requestID)
				t.udpRequestsMu.Unlock()
			} else {
				log.Printf("Invalid handler type for UDP response with ID %s", requestID)
			}
		} else {
			log.Printf("Received unknown UDP message type from %s: %s", remoteAddr, message)
		}
	}
}
