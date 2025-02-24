package p2p

// HandshakeFunc... ?
type HandshakeFunc func(Peer) error

// for testing
func NOPHandshakeFunc(Peer) error { return nil }
