package p2p

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
)

// Tracker stores metadata and peer information for each file
type Tracker struct {
	// Map of File ID to a list of peer addresses sharing the file
	fileIndex map[string]map[string]bool // fileID -> peer address set
	mu        sync.Mutex
}

func (t *TCPTransport) SetOnPeer(handler func(Peer) error) {
	t.OnPeer = handler
}

// NewTracker initializes a new tracker
func NewTracker() *Tracker {
	return &Tracker{
		fileIndex: make(map[string]map[string]bool),
	}
}

// StartTracker starts the tracker server on the specified address
func (t *Tracker) StartTracker(address string) {
	http.HandleFunc("/announce", t.handleAnnounce)
	http.HandleFunc("/peers", t.handleGetPeers)

	log.Printf("Tracker listening on %s", address)
	if err := http.ListenAndServe(address, nil); err != nil {
		log.Fatalf("Failed to start tracker: %v", err)
	}
}

// handleAnnounce handles peers announcing the files they have
// Endpoint: /announce?file_id=<fileID>&peer_addr=<peerAddr>
func (t *Tracker) handleAnnounce(w http.ResponseWriter, r *http.Request) {
	fileID := r.URL.Query().Get("file_id")
	peerAddr := r.URL.Query().Get("peer_addr")

	if fileID == "" || peerAddr == "" {
		http.Error(w, "file_id and peer_addr are required", http.StatusBadRequest)
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	// Add the peer to the list of peers for this fileID
	if _, exists := t.fileIndex[fileID]; !exists {
		t.fileIndex[fileID] = make(map[string]bool)
	}
	t.fileIndex[fileID][peerAddr] = true

	log.Printf("Announced file %s from peer %s", fileID, peerAddr) // Debug log
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "Announced file %s from peer %s\n", fileID, peerAddr)
}

func (t *Tracker) handleGetPeers(w http.ResponseWriter, r *http.Request) {
	fileID := r.URL.Query().Get("file_id")

	if fileID == "" {
		http.Error(w, "file_id is required", http.StatusBadRequest)
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	// Get the list of peers for this fileID
	peers, exists := t.fileIndex[fileID]
	if !exists {
		log.Printf("No peers found for file %s", fileID) // Debug log
		http.Error(w, "No peers found for this file", http.StatusNotFound)
		return
	}

	// Convert the peer map to a list for easier JSON encoding
	peerList := make([]string, 0, len(peers))
	for peer := range peers {
		peerList = append(peerList, peer)
	}

	log.Printf("Peers for file %s: %v", fileID, peerList) // Debug log
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(peerList); err != nil {
		http.Error(w, "Failed to encode peer list", http.StatusInternalServerError)
	}
}

// RemovePeer allows a peer to be removed from all file listings when it disconnects
func (t *Tracker) RemovePeer(peerAddr string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for fileID, peers := range t.fileIndex {
		if peers[peerAddr] {
			delete(peers, peerAddr)
			log.Printf("Removed peer %s from file %s", peerAddr, fileID)

			// Clean up empty entries
			if len(peers) == 0 {
				delete(t.fileIndex, fileID)
			}
		}
	}
}
