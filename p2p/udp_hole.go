package p2p

import (
	"fmt"
	"log"
	"net"
	"strings"
	"time"

	"github.com/anthdm/foreverstore/shared"
	"math/rand"
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
	buf := make([]byte, 1024)

	for {
		n, remoteAddr, err := t.udpConn.ReadFromUDP(buf)
		if err != nil {
			log.Printf("UDP read error: %v", err)
			continue
		}

		message := string(buf[:n])
		log.Printf("Received UDP message from %s: %s", remoteAddr, message)

		if strings.HasPrefix(message, "PUNCH") {
			peerAddr := message[5:] // Skip "PUNCH" prefix
			log.Printf("Received PUNCH from %s for TCP address %s", remoteAddr, peerAddr)

			// Get our public IP for the ACK message
			publicIP, err := shared.GetPublicIP()
			if err != nil {
				log.Printf("Warning: Could not get public IP: %v", err)
				localIP, err := shared.GetLocalIP()
				if err != nil {
					log.Printf("Error: Could not get any IP: %v", err)
					continue
				}
				publicIP = localIP
			}

			// Send ACK with our full address
			_, port, _ := net.SplitHostPort(t.ListenAddr)
			ourAddr := net.JoinHostPort(publicIP, port)
			ackMsg := fmt.Sprintf("ACK%s", ourAddr)
			_, err = t.udpConn.WriteToUDP([]byte(ackMsg), remoteAddr)
			if err != nil {
				log.Printf("Failed to send ACK to %s: %v", remoteAddr, err)
				continue
			}
			log.Printf("Sent ACK to %s", remoteAddr)

			// Try both accepting and connecting in parallel
			go func() {
				// Wait a small random time before attempting connection
				time.Sleep(time.Duration(rand.Int63n(500)) * time.Millisecond)

				// Try to establish TCP connection
				conn, err := net.DialTimeout("tcp", peerAddr, 5*time.Second)
				if err == nil {
					log.Printf("Successfully established outbound TCP connection to %s", peerAddr)
					go t.handleConn(conn, true)
				} else {
					log.Printf("Failed to establish TCP connection to %s: %v", peerAddr, err)
				}
			}()

		} else if strings.HasPrefix(message, "ACK") {
			peerAddr := message[3:] // Skip "ACK" prefix
			log.Printf("Received ACK from %s for TCP address %s", remoteAddr, peerAddr)

			// Try a TCP connection after receiving ACK
			go func() {
				// Wait a small random time before attempting connection
				time.Sleep(time.Duration(rand.Int63n(500)) * time.Millisecond)

				// Try to establish TCP connection
				conn, err := net.DialTimeout("tcp", peerAddr, 5*time.Second)
				if err == nil {
					log.Printf("Successfully established TCP connection to %s after ACK", peerAddr)
					go t.handleConn(conn, true)
					select {
					case t.connectedCh <- peerAddr:
						log.Printf("Notified of successful connection to %s", peerAddr)
					default:
						log.Printf("Failed to notify of connection to %s (channel full)", peerAddr)
					}
				} else {
					log.Printf("Failed to establish TCP connection to %s: %v", peerAddr, err)
				}
			}()
		}
	}
}
