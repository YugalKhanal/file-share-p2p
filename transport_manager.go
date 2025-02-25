package main

import (
	"fmt"
	"log"
	"sync"

	"github.com/anthdm/foreverstore/p2p"
)

// TransportManager handles multiple transport types
type TransportManager struct {
	transports []p2p.Transport
	rpcch      chan p2p.RPC
	mu         sync.RWMutex
}

// NewTransportManager creates a new transport manager with the given transports
func NewTransportManager(transports ...p2p.Transport) *TransportManager {
	tm := &TransportManager{
		transports: transports,
		rpcch:      make(chan p2p.RPC, 1024),
	}

	// Start consuming from all transports
	for _, t := range transports {
		go tm.forwardMessages(t)
	}

	return tm
}

// forwardMessages forwards messages from a transport to the main RPC channel
func (tm *TransportManager) forwardMessages(t p2p.Transport) {
	for rpc := range t.Consume() {
		select {
		case tm.rpcch <- rpc:
		default:
			log.Printf("Warning: RPC channel full, dropping message")
		}
	}
}

// Addr returns the address of the first transport
func (tm *TransportManager) Addr() string {
	if len(tm.transports) > 0 {
		return tm.transports[0].Addr()
	}
	return ""
}

// Consume returns the RPC channel
func (tm *TransportManager) Consume() <-chan p2p.RPC {
	return tm.rpcch
}

// Dial attempts to dial using all transports until one succeeds
// Dial attempts to dial using all transports until one succeeds
func (tm *TransportManager) Dial(addr string) error {
	log.Printf("TransportManager attempting to dial %s", addr)

	// First try TCP transport (usually index 0)
	if len(tm.transports) > 0 {
		tcpTransport := tm.transports[0]
		log.Printf("Trying TCP transport first")
		if err := tcpTransport.Dial(addr); err == nil {
			log.Printf("TCP connection successful")
			return nil
		} else {
			log.Printf("TCP connection failed: %v", err)
		}
	}

	// If TCP fails and we have UDP transport, try that
	if len(tm.transports) > 1 {
		udpTransport := tm.transports[1]
		log.Printf("Trying UDP transport")
		if err := udpTransport.Dial(addr); err == nil {
			log.Printf("UDP connection successful")
			return nil
		} else {
			log.Printf("UDP connection failed: %v", err)
		}
	}

	// If all transports fail
	return fmt.Errorf("all transport connection attempts failed")
}

// ListenAndAccept starts listening on all transports
func (tm *TransportManager) ListenAndAccept() error {
	for _, t := range tm.transports {
		if err := t.ListenAndAccept(); err != nil {
			return err
		}
	}
	return nil
}

// Close closes all transports
func (tm *TransportManager) Close() error {
	var lastErr error
	for _, t := range tm.transports {
		if err := t.Close(); err != nil {
			lastErr = err
		}
	}
	return lastErr
}
