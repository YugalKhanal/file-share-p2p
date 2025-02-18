package p2p

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/pion/stun"
)

const (
	udpKeepAliveInterval = 15 * time.Second
	natCheckTimeout      = 10 * time.Second
	publicSTUNServer     = "stun.l.google.com:19302"
)

// PeerEndpoint represents connection information for a peer
type PeerEndpoint struct {
	ID          string
	PublicAddr  *net.UDPAddr
	PrivateAddr *net.UDPAddr
	LastSeen    time.Time
}

// NATService handles NAT traversal functionality
type NATService struct {
	publicAddr   *net.UDPAddr
	privateAddr  *net.UDPAddr
	udpConn      *net.UDPConn
	tcpTransport *TCPTransport
	peers        map[string]*PeerEndpoint
	mu           sync.RWMutex
	stunClient   *stun.Client
	trackerAddr  string
	closeChan    chan struct{}
}

// NewNATService creates a new NAT traversal service
func NewNATService(tcpTransport *TCPTransport, trackerAddr string) (*NATService, error) {
	// Create UDP listener on random port
	udpAddr, err := net.ResolveUDPAddr("udp4", ":0")
	if err != nil {
		return nil, fmt.Errorf("failed to resolve UDP address: %v", err)
	}

	conn, err := net.ListenUDP("udp4", udpAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to create UDP listener: %v", err)
	}

	service := &NATService{
		tcpTransport: tcpTransport,
		udpConn:      conn,
		peers:        make(map[string]*PeerEndpoint),
		trackerAddr:  trackerAddr,
		closeChan:    make(chan struct{}),
	}

	// Initialize STUN client
	client := stun.NewClient()
	service.stunClient = client

	// Start NAT detection
	if err := service.detectNAT(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("NAT detection failed: %v", err)
	}

	// Start UDP message handling
	go service.handleUDP()
	go service.maintainConnections()

	return service, nil
}

func (n *NATService) detectNAT() error {
	// Create context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), natCheckTimeout)
	defer cancel()

	// Create a channel for the result
	resultChan := make(chan error, 1)

	go func() {
		// Create STUN message
		message := stun.MustBuild(stun.TransactionID, stun.BindingRequest)

		// Send request to STUN server
		conn, err := net.Dial("udp4", publicSTUNServer)
		if err != nil {
			resultChan <- fmt.Errorf("failed to connect to STUN server: %v", err)
			return
		}
		defer conn.Close()

		udpConn := conn.(*net.UDPConn)

		// Send the message
		_, err = udpConn.Write(message.Raw)
		if err != nil {
			resultChan <- fmt.Errorf("failed to send STUN request: %v", err)
			return
		}

		// Read response
		buffer := make([]byte, 1024)
		n, err := udpConn.Read(buffer)
		if err != nil {
			resultChan <- fmt.Errorf("failed to read STUN response: %v", err)
			return
		}

		// Decode response
		response := &stun.Message{Raw: buffer[:n]}
		if err := response.Decode(); err != nil {
			resultChan <- fmt.Errorf("failed to decode STUN response: %v", err)
			return
		}

		var xorAddr stun.XORMappedAddress
		if err := xorAddr.GetFrom(response); err != nil {
			resultChan <- fmt.Errorf("failed to get address from STUN response: %v", err)
			return
		}

		// Store the addresses
		n.publicAddr = &net.UDPAddr{
			IP:   xorAddr.IP,
			Port: xorAddr.Port,
		}
		n.privateAddr = n.udpConn.LocalAddr().(*net.UDPAddr)

		log.Printf("NAT Detection - Public: %s, Private: %s",
			n.publicAddr.String(), n.privateAddr.String())

		resultChan <- nil
	}()

	// Wait for result or timeout
	select {
	case err := <-resultChan:
		return err
	case <-ctx.Done():
		return fmt.Errorf("NAT detection timed out")
	}
}

func (n *NATService) handleUDP() {
	buffer := make([]byte, 2048)
	for {
		select {
		case <-n.closeChan:
			return
		default:
			n.udpConn.SetReadDeadline(time.Now().Add(1 * time.Second))
			size, remoteAddr, err := n.udpConn.ReadFromUDP(buffer)
			if err != nil {
				if !isTimeout(err) {
					log.Printf("UDP read error: %v", err)
				}
				continue
			}

			// Handle hole punch messages
			var msg holePunchMessage
			if err := json.Unmarshal(buffer[:size], &msg); err != nil {
				log.Printf("Failed to unmarshal hole punch message: %v", err)
				continue
			}

			n.handleHolePunchMessage(msg, remoteAddr)
		}
	}
}

// NewNATService creates a new NAT traversal service

func (n *NATService) handleSTUNMessage(e stun.Event) {
	if e.Error != nil {
		log.Printf("STUN error: %v", e.Error)
		return
	}

	var xorAddr stun.XORMappedAddress
	if err := xorAddr.GetFrom(e.Message); err != nil {
		log.Printf("Failed to get address from STUN message: %v", err)
		return
	}

	log.Printf("Received STUN mapping: %s", xorAddr.String())
}

type holePunchMessage struct {
	Type        string `json:"type"`
	PeerID      string `json:"peer_id"`
	PublicAddr  string `json:"public_addr"`
	PrivateAddr string `json:"private_addr"`
}

func (n *NATService) handleHolePunchMessage(msg holePunchMessage, remoteAddr *net.UDPAddr) {
	n.mu.Lock()
	defer n.mu.Unlock()

	switch msg.Type {
	case "punch":
		// Send acknowledgment
		response := holePunchMessage{
			Type:        "punch_ack",
			PeerID:      n.tcpTransport.ListenAddr,
			PublicAddr:  n.publicAddr.String(),
			PrivateAddr: n.privateAddr.String(),
		}

		data, err := json.Marshal(response)
		if err != nil {
			log.Printf("Failed to marshal punch response: %v", err)
			return
		}

		n.udpConn.WriteToUDP(data, remoteAddr)

	case "punch_ack":
		// Update peer information
		pubAddr, err := net.ResolveUDPAddr("udp", msg.PublicAddr)
		if err != nil {
			log.Printf("Failed to resolve peer public address: %v", err)
			return
		}

		privAddr, err := net.ResolveUDPAddr("udp", msg.PrivateAddr)
		if err != nil {
			log.Printf("Failed to resolve peer private address: %v", err)
			return
		}

		n.peers[msg.PeerID] = &PeerEndpoint{
			ID:          msg.PeerID,
			PublicAddr:  pubAddr,
			PrivateAddr: privAddr,
			LastSeen:    time.Now(),
		}
	}
}

func (n *NATService) maintainConnections() {
	ticker := time.NewTicker(udpKeepAliveInterval)
	defer ticker.Stop()

	for {
		select {
		case <-n.closeChan:
			return
		case <-ticker.C:
			n.mu.RLock()
			for _, peer := range n.peers {
				// Send hole punch to both addresses
				n.sendHolePunch(peer.PublicAddr)
				n.sendHolePunch(peer.PrivateAddr)
			}
			n.mu.RUnlock()
		}
	}
}

func (n *NATService) sendHolePunch(addr *net.UDPAddr) {
	msg := holePunchMessage{
		Type:        "punch",
		PeerID:      n.tcpTransport.ListenAddr,
		PublicAddr:  n.publicAddr.String(),
		PrivateAddr: n.privateAddr.String(),
	}

	data, err := json.Marshal(msg)
	if err != nil {
		log.Printf("Failed to marshal hole punch message: %v", err)
		return
	}

	if _, err := n.udpConn.WriteToUDP(data, addr); err != nil {
		log.Printf("Failed to send hole punch to %s: %v", addr.String(), err)
	}
}

func (n *NATService) InitiateConnection(peerAddr string) error {
	// Try both public and private addresses
	addrs := []string{peerAddr}

	n.mu.RLock()
	if peer, ok := n.peers[peerAddr]; ok {
		addrs = append(addrs, peer.PublicAddr.String(), peer.PrivateAddr.String())
	}
	n.mu.RUnlock()

	// Send hole punches to all possible addresses
	for _, addr := range addrs {
		udpAddr, err := net.ResolveUDPAddr("udp", addr)
		if err != nil {
			continue
		}
		n.sendHolePunch(udpAddr)
	}

	// Wait briefly for hole punch to work
	time.Sleep(500 * time.Millisecond)
	return nil
}

func (n *NATService) Close() error {
	close(n.closeChan)
	if n.stunClient != nil {
		n.stunClient.Close()
	}
	return n.udpConn.Close()
}

func isTimeout(err error) bool {
	if netErr, ok := err.(net.Error); ok {
		return netErr.Timeout()
	}
	return false
}
