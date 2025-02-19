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
	buf := make([]byte, 2048)

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

		if strings.HasPrefix(message, "PUNCH") {
			parts := strings.Split(message[5:], "|") // Skip "PUNCH" prefix
			if len(parts) != 2 {
				log.Printf("Invalid PUNCH message format from %s", remoteAddr)
				continue
			}

			peerAddr := parts[0]
			msgID := parts[1]

			log.Printf("Received PUNCH from %s for address %s (ID: %s)", remoteAddr, peerAddr, msgID)

			// Get our public IP for the ACK message
			publicIP, err := shared.GetPublicIP()
			if err != nil {
				localIP, err := shared.GetLocalIP()
				if err != nil {
					log.Printf("Error: Could not get any IP: %v", err)
					continue
				}
				publicIP = localIP
			}

			// Send ACK messages
			_, port, _ := net.SplitHostPort(t.ListenAddr)
			ourAddr := net.JoinHostPort(publicIP, port)

			go func() {
				for i := 0; i < 3; i++ {
					ackMsg := fmt.Sprintf("ACK%s|%s", ourAddr, msgID)
					if _, err := t.udpConn.WriteToUDP([]byte(ackMsg), remoteAddr); err != nil {
						log.Printf("Failed to send ACK to %s: %v", remoteAddr, err)
						continue
					}
					log.Printf("Sent ACK %d/3 to %s", i+1, remoteAddr)
					time.Sleep(100 * time.Millisecond)
				}
			}()

			// Notify of successful hole punch
			select {
			case t.connectedCh <- peerAddr:
				log.Printf("UDP hole punch established with %s", peerAddr)
			default:
				log.Printf("Channel full, but UDP hole punch established with %s", peerAddr)
			}
		} else if strings.HasPrefix(message, "ACK") {
			parts := strings.Split(message[3:], "|") // Skip "ACK" prefix
			if len(parts) != 2 {
				log.Printf("Invalid ACK message format from %s", remoteAddr)
				continue
			}

			// Call ACK handler if registered
			if handler, ok := t.punchingMap.Load("ackHandler"); ok {
				if fn, ok := handler.(func(*net.UDPAddr)); ok {
					fn(remoteAddr)
				}
			}
		}
	}
}
