package p2p

import (
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"time"
)

// RequestUDPData sends a data request over UDP
func (t *TCPTransport) RequestUDPData(peerAddr, fileID string, chunkIndex int, requestID string) error {
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
func (t *TCPTransport) RegisterUDPResponseHandler(requestID string, handler func([]byte, error)) {
	t.udpResponseHandlers.Store(requestID, handler)
}

// UnregisterUDPResponseHandler removes a response handler
func (t *TCPTransport) UnregisterUDPResponseHandler(requestID string) {
	t.udpResponseHandlers.Delete(requestID)

	// Also clean up the request
	t.udpRequestsMu.Lock()
	delete(t.udpRequests, requestID)
	t.udpRequestsMu.Unlock()
}

// StartUDPRequestTimeoutChecker monitors and retries UDP requests
func (t *TCPTransport) StartUDPRequestTimeoutChecker() {
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

func (t *TCPTransport) setupUDPListener() error {
	log.Printf("Setting up UDP listener on port %d", t.getPort())

	udpAddr := &net.UDPAddr{
		IP:   net.IPv4zero,
		Port: t.getPort(),
	}

	var err error
	t.udpConn, err = net.ListenUDP("udp", udpAddr)
	if err != nil {
		return fmt.Errorf("UDP listen failed: %v", err)
	}

	log.Printf("UDP listener established successfully on %s", t.udpConn.LocalAddr())
	go t.handleUDPMessages()
	return nil
}

// Setup UDP data channel for file transfer
func (t *TCPTransport) setupUDPDataChannel(remoteAddr *net.UDPAddr) {
	log.Printf("UDP data channel established with %s", remoteAddr)

	// Notify our FileServer that this peer is available via UDP
	// (This would require extending the FileServer to handle UDP peers)
	if t.OnUDPPeer != nil {
		t.OnUDPPeer(remoteAddr.String())
	}
}

func (t *TCPTransport) handleUDPMessages() {
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

			// Process request in a separate goroutine to avoid blocking
			go func() {
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

					// Then send the data (might need to break this up into smaller packets for large chunks)
					_, err = t.udpConn.WriteToUDP(chunkData, remoteAddr)
					if err != nil {
						log.Printf("Failed to send UDP data response: %v", err)
					} else {
						log.Printf("Successfully sent chunk %d (%d bytes) via UDP", chunkIndex, len(chunkData))
					}
				} else {
					log.Printf("Cannot handle UDP data request: no data request handler registered")
				}
			}()
		} else if strings.HasPrefix(message, "DATA_RESP:") {
			// Handle data response: DATA_RESP:<requestID>:<fileID>:<chunkIndex>:<data>
			// First extract the header
			headerEndIndex := strings.Index(message, "DATA_RESP:")
			if headerEndIndex == -1 {
				log.Printf("Invalid DATA_RESP format from %s: header not found", remoteAddr)
				continue
			}

			headerParts := strings.Split(message, ":")
			if len(headerParts) < 4 {
				log.Printf("Invalid DATA_RESP header parts: %v", headerParts)
				continue
			}

			requestID := headerParts[1]
			fileID := headerParts[2]
			chunkIndex, err := strconv.Atoi(headerParts[3])
			if err != nil {
				log.Printf("Invalid chunk index in DATA_RESP: %s", headerParts[3])
				continue
			}

			log.Printf("Received UDP data response header for requestID: %s, file: %s, chunk: %d",
				requestID, fileID, chunkIndex)

			// Extract data part - everything after the header and the 4th colon
			headerLen := len(fmt.Sprintf("DATA_RESP:%s:%s:%d:", requestID, fileID, chunkIndex))
			data := buf[headerLen:n]

			log.Printf("Extracted data of length %d bytes", len(data))

			// Look up the handler for this response
			handlerObj, exists := t.udpResponseHandlers.Load(requestID)
			if !exists {
				log.Printf("No handler found for UDP response with ID %s", requestID)
				continue
			}

			// Call the handler
			if handler, ok := handlerObj.(func([]byte, error)); ok {
				log.Printf("Calling response handler for requestID: %s", requestID)
				handler(data, nil)

				// Clean up after successful response
				t.udpResponseHandlers.Delete(requestID)
				t.udpRequestsMu.Lock()
				delete(t.udpRequests, requestID)
				t.udpRequestsMu.Unlock()

				log.Printf("Completed UDP data response handling for requestID: %s", requestID)
			} else {
				log.Printf("Invalid handler type for UDP response with ID %s", requestID)
			}
		} else {
			log.Printf("Received unknown UDP message type from %s: %s", remoteAddr, message)
		}
	}
}
