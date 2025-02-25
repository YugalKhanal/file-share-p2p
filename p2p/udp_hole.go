package p2p

import (
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

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

// Setup UDP data channel for file transfer
func (t *TCPTransport) setupUDPDataChannel(remoteAddr *net.UDPAddr) {
	log.Printf("UDP data channel established with %s", remoteAddr)

	// Notify our FileServer that this peer is available via UDP
	// (This would require extending the FileServer to handle UDP peers)
	if t.OnUDPPeer != nil {
		t.OnUDPPeer(remoteAddr.String())
	}
}

// RequestUDPData sends a data request over UDP
func (t *TCPTransport) RequestUDPData(peerAddr, fileID string, chunkIndex int, requestID string) error {
	log.Printf("Entering RequestUDPData for peer %s, file %s, chunk %d, requestID %s",
		peerAddr, fileID, chunkIndex, requestID)

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
	log.Printf("Sending UDP request message: %s to %s", reqMsg, udpAddr)

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
		Handler:    t.getResponseHandler(requestID),
		Timestamp:  time.Now(),
		Attempts:   1,
	}
	t.udpRequestsMu.Unlock()

	return nil
}

// New helper function to get the response handler
func (t *TCPTransport) getResponseHandler(requestID string) func([]byte, error) {
	value, exists := t.udpResponseHandlers.Load(requestID)
	if !exists {
		log.Printf("Warning: No response handler found for request ID %s", requestID)
		return nil
	}

	handler, ok := value.(func([]byte, error))
	if !ok {
		log.Printf("Warning: Invalid handler type for request ID %s", requestID)
		return nil
	}

	return handler
}

// Handle UDP data request
func (t *TCPTransport) handleUDPDataRequest(remoteAddr *net.UDPAddr, fileID string, chunkIndex int, requestID string) {
	log.Printf("Received UDP data request for file %s chunk %d from %s (requestID: %s)",
		fileID, chunkIndex, remoteAddr, requestID)

	if t.OnUDPDataRequest != nil {
		log.Printf("Calling OnUDPDataRequest callback to get chunk data")
		chunkData, err := t.OnUDPDataRequest(fileID, chunkIndex)
		if err != nil {
			log.Printf("Failed to fetch chunk data: %v", err)
			// Send error response
			errMsg := fmt.Sprintf("ERROR_RESP:%s:%s", requestID, err.Error())
			t.udpConn.WriteToUDP([]byte(errMsg), remoteAddr)
			return
		}

		log.Printf("Successfully got chunk data: %d bytes", len(chunkData))

		// Send data response
		dataResp := fmt.Sprintf("DATA_RESP:%s:%s:%d:", requestID, fileID, chunkIndex)
		log.Printf("Sending data response header: %s", dataResp)

		// Combine header and data
		fullMsg := make([]byte, len(dataResp)+len(chunkData))
		copy(fullMsg, []byte(dataResp))
		copy(fullMsg[len(dataResp):], chunkData)

		// Send in a single packet if possible
		if len(fullMsg) <= 65507 { // Max UDP packet size
			_, err = t.udpConn.WriteToUDP(fullMsg, remoteAddr)
			if err != nil {
				log.Printf("Failed to send UDP data response: %v", err)
			} else {
				log.Printf("Successfully sent chunk %d (%d bytes) via UDP",
					chunkIndex, len(fullMsg))
			}
		} else {
			// For large responses, implement chunking
			log.Printf("Data too large for single UDP packet, would need to implement chunking")
			// For now, send an error
			errMsg := fmt.Sprintf("ERROR_RESP:%s:data_too_large", requestID)
			t.udpConn.WriteToUDP([]byte(errMsg), remoteAddr)
		}
	} else {
		log.Printf("Cannot handle UDP data request: no data request handler registered")
	}
}

func (t *TCPTransport) handleUDPMessages() {
	log.Printf("Starting UDP message handler")
	buf := make([]byte, 64*1024) // Large buffer for data chunks

	// Map to store multi-part transfers
	multipartData := make(map[string][]byte)
	multipartMeta := make(map[string]map[string]int)
	var multipartMutex sync.Mutex

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
			// Handle single-packet data response
			prefixLen := len("DATA_RESP:")
			rest := message[prefixLen:]

			// Find the third colon
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

			if headerEnd == 0 || headerEnd >= n {
				log.Printf("Invalid DATA_RESP format from %s", remoteAddr)
				continue
			}

			header := message[:headerEnd]
			parts := strings.Split(header, ":")
			if len(parts) < 4 {
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

			// Extract data
			data := buf[headerEnd:n]

			log.Printf("Received UDP data response for file %s chunk %d from %s (%d bytes)",
				fileID, chunkIndex, remoteAddr, len(data))

			// Look up handler and call it
			handlerObj, exists := t.udpResponseHandlers.Load(requestID)
			if !exists {
				log.Printf("No handler found for UDP response with ID %s", requestID)
				continue
			}

			if handler, ok := handlerObj.(func([]byte, error)); ok {
				handler(data, nil)

				// Clean up
				t.udpResponseHandlers.Delete(requestID)
				t.udpRequestsMu.Lock()
				delete(t.udpRequests, requestID)
				t.udpRequestsMu.Unlock()
			}

		} else if strings.HasPrefix(message, "PART:") {
			// Handle multi-packet transfer part
			parts := strings.Split(message, ":")
			if len(parts) < 5 {
				log.Printf("Invalid PART format from %s", remoteAddr)
				continue
			}

			requestID := parts[1]
			offset, _ := strconv.Atoi(parts[2])
			length, _ := strconv.Atoi(parts[3])

			// Find data start position
			dataStart := len(parts[0]) + len(requestID) + len(parts[2]) + len(parts[3]) + 4 // 4 colons

			if dataStart >= n || dataStart+length > n {
				log.Printf("Invalid PART data dimensions: start=%d, length=%d, message_size=%d",
					dataStart, length, n)
				continue
			}

			// Extract data part
			dataPart := buf[dataStart : dataStart+length]

			// Store in multipart map
			multipartMutex.Lock()
			if _, exists := multipartData[requestID]; !exists {
				multipartData[requestID] = make([]byte, 0)
				multipartMeta[requestID] = make(map[string]int)
			}

			// Ensure we have enough capacity
			if offset+length > len(multipartData[requestID]) {
				newData := make([]byte, offset+length)
				copy(newData, multipartData[requestID])
				multipartData[requestID] = newData
			}

			// Copy data part at the right offset
			copy(multipartData[requestID][offset:], dataPart)

			// Update metadata
			multipartMeta[requestID]["lastOffset"] = offset + length
			multipartMeta[requestID]["parts"]++

			log.Printf("Received part %d for request %s (%d bytes at offset %d)",
				multipartMeta[requestID]["parts"], requestID, length, offset)
			multipartMutex.Unlock()

		} else if strings.HasPrefix(message, "COMPLETE:") {
			// Handle completion of multi-packet transfer
			parts := strings.Split(message, ":")
			if len(parts) < 2 {
				log.Printf("Invalid COMPLETE format from %s", remoteAddr)
				continue
			}

			requestID := parts[1]

			multipartMutex.Lock()
			data, exists := multipartData[requestID]
			meta, metaExists := multipartMeta[requestID]

			if !exists || !metaExists {
				log.Printf("No multipart data found for request ID %s", requestID)
				multipartMutex.Unlock()
				continue
			}

			// Trim to actual size if needed
			actualSize := meta["lastOffset"]
			if actualSize < len(data) {
				data = data[:actualSize]
			}

			log.Printf("Completed multipart transfer for request %s (%d bytes in %d parts)",
				requestID, len(data), meta["parts"])

			// Look up handler
			handlerObj, handlerExists := t.udpResponseHandlers.Load(requestID)

			// Clean up multipart data
			delete(multipartData, requestID)
			delete(multipartMeta, requestID)
			multipartMutex.Unlock()

			// Call handler if exists
			if handlerExists {
				if handler, ok := handlerObj.(func([]byte, error)); ok {
					handler(data, nil)

					// Clean up
					t.udpResponseHandlers.Delete(requestID)
					t.udpRequestsMu.Lock()
					delete(t.udpRequests, requestID)
					t.udpRequestsMu.Unlock()
				}
			}

		} else if strings.HasPrefix(message, "ERROR_RESP:") {
			// Handle error responses
			parts := strings.Split(strings.TrimPrefix(message, "ERROR_RESP:"), ":")
			if len(parts) < 2 {
				log.Printf("Invalid ERROR_RESP format from %s", remoteAddr)
				continue
			}

			requestID := parts[0]
			errorMsg := parts[1]

			log.Printf("Received error response for request %s: %s", requestID, errorMsg)

			handlerObj, exists := t.udpResponseHandlers.Load(requestID)
			if exists {
				if handler, ok := handlerObj.(func([]byte, error)); ok {
					handler(nil, fmt.Errorf("remote error: %s", errorMsg))
				}

				// Clean up
				t.udpResponseHandlers.Delete(requestID)
				t.udpRequestsMu.Lock()
				delete(t.udpRequests, requestID)
				t.udpRequestsMu.Unlock()
			}

		} else {
			log.Printf("Received unknown UDP message type from %s: %s", remoteAddr, message)
		}
	}
}
