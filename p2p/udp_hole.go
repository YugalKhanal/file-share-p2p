package p2p

import (
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"time"
)

// Request data via UDP
// RequestUDPData sends a data request over UDP
func (t *TCPTransport) RequestUDPData(peerAddr, fileID string, chunkIndex int, requestID string) error {
	// Get the UDPAddr for this peer
	addrObj, ok := t.udpPeers.Load(peerAddr)
	if !ok {
		return fmt.Errorf("unknown UDP peer: %s", peerAddr)
	}

	udpAddr, ok := addrObj.(*net.UDPAddr)
	if !ok {
		return fmt.Errorf("invalid UDP peer address type")
	}

	// Create request message
	// Format: DATA_REQ:<requestID>:<fileID>:<chunkIndex>
	reqMsg := fmt.Sprintf("DATA_REQ:%s:%s:%d", requestID, fileID, chunkIndex)

	// Send the request
	_, err := t.udpConn.WriteToUDP([]byte(reqMsg), udpAddr)
	if err != nil {
		return fmt.Errorf("failed to send UDP data request: %v", err)
	}

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

	log.Printf("Sent UDP data request for file %s chunk %d to %s (requestID: %s)",
		fileID, chunkIndex, udpAddr, requestID)

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
			// Handle PUNCH messages (existing code)
		} else if strings.HasPrefix(message, "ACK:") {
			// Handle ACK messages (existing code)
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
			go func() {
				// Get the file data (this would be implemented by FileServer)
				// For now, simulate a response
				chunkData := []byte("SIMULATED CHUNK DATA " + strconv.Itoa(chunkIndex))

				// Send response: DATA_RESP:<requestID>:<fileID>:<chunkIndex>:<data>
				respMsg := fmt.Sprintf("DATA_RESP:%s:%s:%d:", requestID, fileID, chunkIndex)

				// Create the full message with binary data
				respBytes := append([]byte(respMsg), chunkData...)

				// Send the response
				_, err := t.udpConn.WriteToUDP(respBytes, remoteAddr)
				if err != nil {
					log.Printf("Failed to send UDP data response: %v", err)
				} else {
					log.Printf("Sent UDP data response for file %s chunk %d (%d bytes)",
						fileID, chunkIndex, len(chunkData))
				}
			}()

		} else if strings.HasPrefix(message, "DATA_RESP:") {
			// Handle data response: DATA_RESP:<requestID>:<fileID>:<chunkIndex>:<data>
			headerEnd := strings.Index(message, ":") + 10 // Find the 4th colon
			if headerEnd == -1 {
				log.Printf("Invalid DATA_RESP format from %s", remoteAddr)
				continue
			}

			header := message[:headerEnd]
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

			// Extract the data part (everything after the 4th colon)
			data := buf[headerEnd+1 : n]

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
