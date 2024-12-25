package p2p

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
)

// TCPPeer represents the remote node over a TCP established connection.
type TCPPeer struct {
	// The underlying connection of the peer. Which in this case
	// is a TCP connection.
	net.Conn
	// if we dial and retrieve a conn => outbound == true
	// if we accept and retrieve a conn => outbound == false
	outbound bool

	wg *sync.WaitGroup
}

func NewTCPPeer(conn net.Conn, outbound bool) *TCPPeer {
	return &TCPPeer{
		Conn:     conn,
		outbound: outbound,
		wg:       &sync.WaitGroup{},
	}
}

func (p *TCPPeer) CloseStream() {
	p.wg.Done()
}

func (p *TCPPeer) Send(b []byte) error {
	_, err := p.Conn.Write(b)
	return err
}

type TCPTransportOpts struct {
	ListenAddr    string
	HandshakeFunc HandshakeFunc
	Decoder       Decoder
	OnPeer        func(Peer) error
}

type TCPTransport struct {
	TCPTransportOpts
	listener net.Listener
	rpcch    chan RPC
}

func NewTCPTransport(opts TCPTransportOpts) *TCPTransport {
	return &TCPTransport{
		TCPTransportOpts: opts,
		rpcch:            make(chan RPC, 1024),
	}
}

// Addr implements the Transport interface return the address
// the transport is accepting connections.
func (t *TCPTransport) Addr() string {
	return t.ListenAddr
}

// Consume implements the Tranport interface, which will return read-only channel
// for reading the incoming messages received from another peer in the network.
func (t *TCPTransport) Consume() <-chan RPC {
	return t.rpcch
}

// Close implements the Transport interface.
func (t *TCPTransport) Close() error {
	return t.listener.Close()
}

// Dial implements the Transport interface.
func (t *TCPTransport) Dial(addr string) error {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return err
	}

	go t.handleConn(conn, true)

	return nil
}

func (t *TCPTransport) ListenAndAccept() error {
	var err error

	t.listener, err = net.Listen("tcp", t.ListenAddr)
	if err != nil {
		return err
	}

	go t.startAcceptLoop()

	log.Printf("TCP transport listening on port: %s\n", t.ListenAddr)

	return nil
}

func (t *TCPTransport) startAcceptLoop() {
	for {
		conn, err := t.listener.Accept()
		if errors.Is(err, net.ErrClosed) {
			return
		}

		if err != nil {
			fmt.Printf("TCP accept error: %s\n", err)
		}

		go t.handleConn(conn, false)
	}
}

// In tcp_transport.go
// In tcp_transport.go
func (t *TCPTransport) handleConn(conn net.Conn, outbound bool) {
	var err error

	// Set up defer for connection cleanup
	defer func() {
		// Log connection closure with any associated error
		if err != nil {
			log.Printf("Closing connection to %s due to error: %v",
				conn.RemoteAddr(), err)
		} else {
			log.Printf("Closing connection to %s (normal shutdown)",
				conn.RemoteAddr())
		}
		conn.Close()
	}()

	// Create and initialize peer
	peer := NewTCPPeer(conn, outbound)
	log.Printf("New peer connection established: remote=%s outbound=%v",
		conn.RemoteAddr(), outbound)

	// Perform handshake
	if err = t.HandshakeFunc(peer); err != nil {
		log.Printf("Handshake failed with peer %s: %v",
			conn.RemoteAddr(), err)
		return
	}
	log.Printf("Handshake completed successfully with peer %s",
		conn.RemoteAddr())

	// Notify about new peer if callback is set
	if t.OnPeer != nil {
		if err = t.OnPeer(peer); err != nil {
			log.Printf("OnPeer callback failed for %s: %v",
				conn.RemoteAddr(), err)
			return
		}
		log.Printf("OnPeer callback completed successfully for %s",
			conn.RemoteAddr())
	}

	// Start the main message reading loop
	for {
		// Create a new RPC message container
		rpc := RPC{}
		log.Printf("Reading new message from peer %s", conn.RemoteAddr())

		// Attempt to decode the incoming message
		err = t.Decoder.Decode(conn, &rpc)
		if err != nil {
			if err == io.EOF {
				log.Printf("Connection closed by peer %s", conn.RemoteAddr())
			} else {
				log.Printf("Error decoding message from %s: %v",
					conn.RemoteAddr(), err)
			}
			return
		}

		// Log successful message decode
		log.Printf("Successfully decoded message from %s: StreamFlag=%v PayloadSize=%d",
			conn.RemoteAddr(), rpc.Stream, len(rpc.Payload))

		// Set message source
		rpc.From = conn.RemoteAddr().String()

		// Try to send the message through the RPC channel
		select {
		case t.rpcch <- rpc:
			log.Printf("Successfully forwarded message from %s to RPC channel",
				conn.RemoteAddr())
		default:
			// Channel is full, log warning and drop message
			log.Printf("Warning: RPC channel full, dropping message from %s",
				conn.RemoteAddr())
		}
	}
}
