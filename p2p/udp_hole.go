package p2p

import (
	"fmt"
	"log"
	"net"
	"strings"
	"time"
)

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
	buf := make([]byte, 1024)

	for {
		n, remoteAddr, err := t.udpConn.ReadFromUDP(buf)
		if err != nil {
			log.Printf("UDP read error: %v", err)
			continue
		}

		message := string(buf[:n])
		log.Printf("Received UDP message from %s: %s", remoteAddr, message)

		parts := strings.Split(message, ":")
		if len(parts) != 2 {
			log.Printf("Invalid message format: %s", message)
			continue
		}

		messageType, tcpAddr := parts[0], parts[1]
		switch messageType {
		case "PUNCH":
			log.Printf("Received PUNCH from %s for TCP address %s", remoteAddr, tcpAddr)

			// Try multiple connection attempts
			for i := 0; i < 3; i++ {
				// Send ACK
				ackMsg := fmt.Sprintf("ACK:%s", t.ListenAddr)
				if _, err := t.udpConn.WriteToUDP([]byte(ackMsg), remoteAddr); err != nil {
					log.Printf("Failed to send ACK to %s: %v", remoteAddr, err)
				} else {
					log.Printf("Sent ACK to %s (attempt %d)", remoteAddr, i+1)
				}

				// Try TCP connection
				go func(attempt int) {
					time.Sleep(time.Duration(attempt*100) * time.Millisecond)
					if conn, err := net.DialTimeout("tcp", tcpAddr, 3*time.Second); err == nil {
						log.Printf("Successfully established TCP connection to %s", tcpAddr)
						go t.handleConn(conn, true)
					}
				}(i)

				time.Sleep(200 * time.Millisecond)
			}

		case "ACK":
			log.Printf("Received ACK from %s for TCP address %s", remoteAddr, tcpAddr)
			select {
			case t.connectedCh <- tcpAddr:
				log.Printf("Notified of successful connection to %s", tcpAddr)
			default:
				log.Printf("Failed to notify of connection to %s (channel full)", tcpAddr)
			}
		}
	}
}
