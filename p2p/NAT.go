package p2p

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/anthdm/foreverstore/shared"
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
	// Get local IP first
	localIP, err := shared.GetLocalIP()
	if err != nil {
		return nil, fmt.Errorf("failed to get local IP: %v", err)
	}

	// Create UDP listener bound to local IP
	udpAddr, err := net.ResolveUDPAddr("udp4", fmt.Sprintf("%s:0", localIP))
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

	// Create STUN client with our UDP connection
	client, err := stun.NewClient(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to create STUN client: %v", err)
	}
	service.stunClient = client

	// Start NAT detection
	if err := service.detectNAT(); err != nil {
		conn.Close()
		client.Close()
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

	n.mu.Lock()
	localAddr := n.udpConn.LocalAddr().(*net.UDPAddr)
	n.privateAddr = localAddr
	n.mu.Unlock()

	go func() {
		// Create STUN message
		message := stun.MustBuild(stun.TransactionID, stun.BindingRequest)

		// Connect to STUN server
		c, err := stun.Dial("udp4", publicSTUNServer)
		if err != nil {
			resultChan <- fmt.Errorf("failed to connect to STUN server: %v", err)
			return
		}
		defer c.Close()

		// Send binding request and wait for response
		if err := c.Do(message, func(res stun.Event) {
			if res.Error != nil {
				resultChan <- res.Error
				return
			}

			// Get XOR-MAPPED-ADDRESS from STUN response
			var xorAddr stun.XORMappedAddress
			if err := xorAddr.GetFrom(res.Message); err != nil {
				resultChan <- err
				return
			}

			// Store the public address
			n.mu.Lock()
			n.publicAddr = &net.UDPAddr{
				IP:   xorAddr.IP,
				Port: xorAddr.Port,
			}
			n.mu.Unlock()

			log.Printf("NAT Detection - Public: %s, Private: %s",
				n.publicAddr.String(), n.privateAddr.String())

			resultChan <- nil
		}); err != nil {
			resultChan <- err
			return
		}
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
					log.Printf("UDP read error from %v: %v", remoteAddr, err)
				}
				continue
			}

			// Log raw packet for debugging
			log.Printf("Received UDP packet of size %d from %s", size, remoteAddr.String())

			// Handle hole punch messages
			var msg holePunchMessage
			if err := json.Unmarshal(buffer[:size], &msg); err != nil {
				log.Printf("Failed to unmarshal hole punch message from %s: %v\nPayload: %s",
					remoteAddr, err, string(buffer[:size]))
				continue
			}

			// Calculate latency
			latency := time.Since(msg.Timestamp)
			log.Printf("Received hole punch message type=%s from %s (latency: %v)",
				msg.Type, remoteAddr.String(), latency)

			go n.handleHolePunchMessage(msg, remoteAddr)
		}
	}
}

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
	Type        string    `json:"type"`
	PeerID      string    `json:"peer_id"`
	PublicAddr  string    `json:"public_addr"`
	PrivateAddr string    `json:"private_addr"`
	Timestamp   time.Time `json:"timestamp"`
	Sequence    int       `json:"sequence"`
}

func (n *NATService) maintainConnections() {
	ticker := time.NewTicker(udpKeepAliveInterval)
	cleanupTicker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	defer cleanupTicker.Stop()

	for {
		select {
		case <-n.closeChan:
			return
		case <-ticker.C:
			n.mu.RLock()
			for _, peer := range n.peers {
				if time.Since(peer.LastSeen) < time.Minute*2 {
					n.sendHolePunch(peer.PublicAddr)
					n.sendHolePunch(peer.PrivateAddr)
				}
			}
			n.mu.RUnlock()
		case <-cleanupTicker.C:
			n.cleanupStaleConnections()
		}
	}
}

func (n *NATService) cleanupStaleConnections() {
	n.mu.Lock()
	defer n.mu.Unlock()

	for id, peer := range n.peers {
		if time.Since(peer.LastSeen) > time.Minute*2 {
			delete(n.peers, id)
			log.Printf("Removed stale peer connection: %s", id)
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

	log.Printf("Sending hole punch to %s (public: %s, private: %s)",
		addr.String(), n.publicAddr.String(), n.privateAddr.String())

	if _, err := n.udpConn.WriteToUDP(data, addr); err != nil {
		log.Printf("Failed to send hole punch to %s: %v", addr.String(), err)
	}
}

func (n *NATService) InitiateConnection(peerAddr string) error {
	addrs := strings.Split(peerAddr, "|")
	if len(addrs) == 0 {
		return fmt.Errorf("invalid peer address format")
	}

	publicAddr := addrs[0]
	log.Printf("Initiating NAT traversal to peer %s", publicAddr)

	udpAddr, err := net.ResolveUDPAddr("udp", publicAddr)
	if err != nil {
		return fmt.Errorf("failed to resolve peer address: %v", err)
	}

	success := make(chan bool, 1)
	sequence := 0

	go func() {
		attempts := 10
		interval := 200 * time.Millisecond

		for i := 0; i < attempts; i++ {
			n.mu.RLock()
			_, established := n.peers[publicAddr]
			n.mu.RUnlock()

			if established {
				success <- true
				return
			}

			// Send bursts with increasing sequence numbers
			for j := 0; j < 3; j++ {
				msg := holePunchMessage{
					Type:        "punch",
					PeerID:      n.tcpTransport.ListenAddr,
					PublicAddr:  n.publicAddr.String(),
					PrivateAddr: n.privateAddr.String(),
					Timestamp:   time.Now(),
					Sequence:    sequence,
				}
				sequence++

				data, err := json.Marshal(msg)
				if err != nil {
					log.Printf("Failed to marshal hole punch message: %v", err)
					continue
				}

				if _, err := n.udpConn.WriteToUDP(data, udpAddr); err != nil {
					log.Printf("Failed to send hole punch to %s: %v", udpAddr.String(), err)
				}

				time.Sleep(50 * time.Millisecond)
			}

			time.Sleep(interval)
			interval = time.Duration(float64(interval) * 1.5)
		}
		success <- false
	}()

	select {
	case result := <-success:
		if result {
			log.Printf("NAT traversal successful to %s", publicAddr)
			return nil
		}
	case <-time.After(5 * time.Second):
		// Fall through to TCP
	}

	return fmt.Errorf("NAT traversal failed, falling back to TCP")
}

func (n *NATService) handleHolePunchMessage(msg holePunchMessage, remoteAddr *net.UDPAddr) {
	n.mu.Lock()
	defer n.mu.Unlock()

	log.Printf("Processing hole punch message: type=%s from=%s seq=%d",
		msg.Type, remoteAddr.String(), msg.Sequence)

	switch msg.Type {
	case "punch":
		go func() {
			response := holePunchMessage{
				Type:        "punch_ack",
				PeerID:      n.tcpTransport.ListenAddr,
				PublicAddr:  n.publicAddr.String(),
				PrivateAddr: n.privateAddr.String(),
				Timestamp:   time.Now(),
				Sequence:    msg.Sequence, // Echo back the sequence
			}

			data, err := json.Marshal(response)
			if err != nil {
				log.Printf("Failed to marshal punch response: %v", err)
				return
			}

			// Send acknowledgments with increasing delays
			delays := []time.Duration{
				0,
				50 * time.Millisecond,
				100 * time.Millisecond,
				200 * time.Millisecond,
				400 * time.Millisecond,
			}

			for _, delay := range delays {
				time.Sleep(delay)
				if _, err := n.udpConn.WriteToUDP(data, remoteAddr); err != nil {
					log.Printf("Failed to send punch_ack: %v", err)
					return
				}
				log.Printf("Sent punch_ack to %s (delay=%v)", remoteAddr.String(), delay)
			}
		}()

	case "punch_ack":
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

		// Store both addresses
		n.peers[msg.PeerID] = &PeerEndpoint{
			ID:          msg.PeerID,
			PublicAddr:  pubAddr,
			PrivateAddr: privAddr,
			LastSeen:    time.Now(),
		}

		// Send confirmation
		response := holePunchMessage{
			Type:        "punch_ack",
			PeerID:      n.tcpTransport.ListenAddr,
			PublicAddr:  n.publicAddr.String(),
			PrivateAddr: n.privateAddr.String(),
		}

		if data, err := json.Marshal(response); err == nil {
			n.udpConn.WriteToUDP(data, remoteAddr)
		}

		log.Printf("Received punch_ack from %s for sequence %d", remoteAddr.String(), msg.Sequence)
		log.Printf("Successfully established NAT traversal with peer %s", msg.PeerID)
	}
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
