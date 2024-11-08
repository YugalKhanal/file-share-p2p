package p2p

func (t *TCPTransport) SetOnPeer(handler func(Peer) error) {
	t.OnPeer = handler
}
