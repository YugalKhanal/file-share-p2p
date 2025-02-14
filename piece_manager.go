package main

import (
	"sort"
	"sync"
)

type PieceStatus int

const (
	PieceMissing PieceStatus = iota
	PieceRequested
	PieceReceived
	PieceVerified
)

type PieceInfo struct {
	Index    int
	Size     int
	Hash     string
	Status   PieceStatus
	Peers    map[string]bool // peers that have this piece
	Priority int             // higher number = higher priority
}

// PieceManager Keeps track of which peers are availalble for each piece
type PieceManager struct {
	pieces    map[int]*PieceInfo
	numPieces int
	mu        sync.RWMutex
}

func NewPieceManager(numPieces int, hashes []string) *PieceManager {
	pm := &PieceManager{
		pieces:    make(map[int]*PieceInfo),
		numPieces: numPieces,
	}

	for i := 0; i < numPieces; i++ {
		pm.pieces[i] = &PieceInfo{
			Index:    i,
			Hash:     hashes[i],
			Status:   PieceMissing,
			Peers:    make(map[string]bool),
			Priority: 0,
		}
	}

	return pm
}

// UpdatePeerPieces updates which pieces a peer has
func (pm *PieceManager) UpdatePeerPieces(peerAddr string, pieceIndices []int) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	// Add peer to each piece's peer list
	for _, idx := range pieceIndices {
		if piece, exists := pm.pieces[idx]; exists {
			piece.Peers[peerAddr] = true

			// Update priority - rarest pieces get highest priority
			piece.Priority = 1000 - len(piece.Peers) // inverse of peer count
		}
	}
}

// RemovePeer removes a peer from all pieces
func (pm *PieceManager) RemovePeer(peerAddr string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	for _, piece := range pm.pieces {
		delete(piece.Peers, peerAddr)
		if piece.Status == PieceRequested {
			piece.Status = PieceMissing // Reset if piece was requested from this peer
		}
		piece.Priority = 1000 - len(piece.Peers)
	}
}

// GetNextPieces returns the next n pieces to download, prioritizing rarest pieces
func (pm *PieceManager) GetNextPieces(n int) []PieceInfo {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	// Get all missing or failed pieces
	candidates := make([]PieceInfo, 0)
	for _, piece := range pm.pieces {
		if piece.Status == PieceMissing && len(piece.Peers) > 0 {
			candidates = append(candidates, *piece)
		}
	}

	// Sort by priority (rarest first)
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Priority > candidates[j].Priority
	})

	// Return up to n pieces
	if len(candidates) > n {
		candidates = candidates[:n]
	}

	// Mark pieces as requested
	for _, piece := range candidates {
		pm.pieces[piece.Index].Status = PieceRequested
	}

	return candidates
}

// MarkPieceStatus updates the status of a piece
func (pm *PieceManager) MarkPieceStatus(index int, status PieceStatus) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if piece, exists := pm.pieces[index]; exists {
		piece.Status = status
	}
}

// IsComplete checks if all pieces are verified
func (pm *PieceManager) IsComplete() bool {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	for _, piece := range pm.pieces {
		if piece.Status != PieceVerified {
			return false
		}
	}
	return true
}
