package p2p

import (
	"fmt"
	"log"
	"net"
	"strings"
	"time"

	"github.com/anthdm/foreverstore/shared"
)

func (t *TCPTransport) setupUDPListener() error {
	log.Printf("Setting up UDP listener on port %d", t.getPort())

	// Try binding to specific addresses
	ips := []net.IP{net.IPv4zero, net.IPv6zero}
	var lastErr error

	for _, ip := range ips {
		udpAddr := &net.UDPAddr{
			IP:   ip,
			Port: t.getPort(),
		}

		conn, err := net.ListenUDP("udp", udpAddr)
		if err != nil {
			lastErr = err
			continue
		}

		// Set UDP socket options
		conn.SetReadBuffer(1024 * 1024)  // 1MB read buffer
		conn.SetWriteBuffer(1024 * 1024) // 1MB write buffer

		t.udpConn = conn
		log.Printf("UDP listener established successfully on %s", t.udpConn.LocalAddr())

		go t.handleUDPMessages()
		return nil
	}

	return fmt.Errorf("failed to setup UDP listener: %v", lastErr)
}

func (t *TCPTransport) handleUDPMessages() {
	log.Printf("Starting UDP message handler")
	buf := make([]byte, 2048) // Increased buffer size

	for {
		n, remoteAddr, err := t.udpConn.ReadFromUDP(buf)
		if err != nil {
			if !strings.Contains(err.Error(), "use of closed network connection") {
				log.Printf("UDP read error: %v", err)
			}
			continue
		}

		message := string(buf[:n])
		log.Printf("Received UDP message from %s: %s", remoteAddr, message)

		// Handle message in a separate goroutine
		go func(msg string, addr *net.UDPAddr) {
			if strings.HasPrefix(msg, "PUNCH") {
				peerAddr := msg[5:] // Skip "PUNCH" prefix
				log.Printf("Received PUNCH from %s for TCP address %s", addr, peerAddr)

				// Get our public IP for the ACK message
				publicIP, err := shared.GetPublicIP()
				if err != nil {
					log.Printf("Failed to get public IP: %v", err)
					localIP, err := shared.GetLocalIP()
					if err != nil {
						log.Printf("Error: Could not get any IP: %v", err)
						return
					}
					publicIP = localIP
				}

				// Send multiple ACK messages to improve reliability
				_, port, _ := net.SplitHostPort(t.ListenAddr)
				ourAddr := net.JoinHostPort(publicIP, port)
				ackMsg := fmt.Sprintf("ACK%s", ourAddr)

				for i := 0; i < 3; i++ {
					_, err = t.udpConn.WriteToUDP([]byte(ackMsg), addr)
					if err != nil {
						log.Printf("Failed to send ACK to %s: %v", addr, err)
						continue
					}
					log.Printf("Sent ACK attempt %d/3 to %s", i+1, addr)
					time.Sleep(100 * time.Millisecond)
				}

				// Try simultaneous TCP connection
				go func() {
					if err := t.attemptSimultaneousConnect(peerAddr); err != nil {
						log.Printf("Simultaneous connection failed: %v", err)
					}
				}()

			} else if strings.HasPrefix(msg, "ACK") {
				peerAddr := msg[3:] // Skip "ACK" prefix
				log.Printf("Received ACK from %s for TCP address %s", addr, peerAddr)

				// Try simultaneous TCP connection
				go func() {
					if err := t.attemptSimultaneousConnect(peerAddr); err != nil {
						log.Printf("Simultaneous connection failed: %v", err)
					} else {
						select {
						case t.connectedCh <- peerAddr:
							log.Printf("Notified of successful connection to %s", peerAddr)
						default:
							log.Printf("Failed to notify of connection to %s (channel full)", peerAddr)
						}
					}
				}()
			}
		}(message, remoteAddr)
	}
}
