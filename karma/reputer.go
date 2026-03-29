// Package karma tracks and propagates reputation scores for nodes.
// Karma is a contribution score that reflects how much compute a node has provided.
// Unlike credits or tokens, karma does NOT affect priority - it's purely for visibility.
package karma

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/compushare/compushare/network/libp2p"
	"github.com/golang/glog"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/peer"
)

// Score represents a node's karma score and history
type Score struct {
	// PeerID is the node's peer ID
	PeerID peer.ID

	// TotalContribution is total milliseconds of compute contributed
	TotalContribution int64

	// TotalRequests is the number of inference requests served
	TotalRequests int

	// SuccessfulRequests is the number of successfully completed requests
	SuccessfulRequests int

	// LastUpdated is the timestamp of the last update
	LastUpdated time.Time

	// Karma is the calculated karma score (0.0 to 1000.0+)
	Karma float64
}

// Reputer manages karma scoring for nodes
type Reputer struct {
	host *libp2p.Host

	// localScores caches karma scores locally
	localScores map[peer.ID]*Score

	// mutex for thread-safe access to localScores
	mu sync.RWMutex

	// persistencePath is where karma scores are stored
	persistencePath string
}

// NewReputer creates a new reputer
func NewReputer(host *libp2p.Host, dataDir string) *Reputer {
	return &Reputer{
		host:            host,
		localScores:     make(map[peer.ID]*Score),
		persistencePath: filepath.Join(dataDir, "karma_scores.json"),
	}
}

// AddContribution records a contribution from a peer
func (r *Reputer) AddContribution(peerID peer.ID, computeTimeMs int64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	score, ok := r.localScores[peerID]
	if !ok {
		score = &Score{
			PeerID: peerID,
			Karma:  1.0, // Start with base karma
		}
		r.localScores[peerID] = score
	}

	score.TotalContribution += computeTimeMs
	score.TotalRequests++
	score.SuccessfulRequests++
	score.LastUpdated = time.Now()

	// Calculate karma per README: Karma = 1.0 + log(total_compute_contributed_ms)
	// math.Log1p(x) = log(1+x), which avoids log(0) when contribution is 0
	score.Karma = 1.0 + math.Log1p(float64(score.TotalContribution))
	if score.Karma < 1.0 {
		score.Karma = 1.0
	}

	glog.V(4).Infof("Updated karma for %s: %.2f (contribution: %d ms)",
		peerID, score.Karma, computeTimeMs)
}

// AddFailure records a failed request from a peer
func (r *Reputer) AddFailure(peerID peer.ID) {
	r.mu.Lock()
	defer r.mu.Unlock()

	score, ok := r.localScores[peerID]
	if !ok {
		score = &Score{PeerID: peerID, Karma: 1.0}
		r.localScores[peerID] = score
	}

	score.TotalRequests++
	score.LastUpdated = time.Now()

	// Reduce karma for failures (but not below 1.0)
	score.Karma = score.Karma * 0.95
	if score.Karma < 1.0 {
		score.Karma = 1.0
	}
}

// GetScore returns the karma score for a peer
func (r *Reputer) GetScore(peerID peer.ID) *Score {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if score, ok := r.localScores[peerID]; ok {
		return score
	}

	// Return default score for unknown peers
	return &Score{
		PeerID: peerID,
		Karma:  1.0,
	}
}

// GetTopProviders returns the top N providers by karma score
func (r *Reputer) GetTopProviders(n int) []*Score {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var scores []*Score
	for _, score := range r.localScores {
		scores = append(scores, score)
	}

	// Sort by karma descending (simple bubble sort for now)
	for i := 0; i < len(scores)-1; i++ {
		for j := i + 1; j < len(scores); j++ {
			if scores[j].Karma > scores[i].Karma {
				scores[i], scores[j] = scores[j], scores[i]
			}
		}
	}

	if len(scores) > n {
		return scores[:n]
	}
	return scores
}

// PropagateScores periodically shares karma scores with the network
func (r *Reputer) PropagateScores(ctx context.Context) {
	ps, err := pubsub.NewGossipSub(ctx, r.host.Underlying())
	if err != nil {
		glog.Errorf("Failed to start GossipSub for karma: %v", err)
		return
	}
	topic, err := ps.Join("compushare:karma")
	if err != nil {
		glog.Errorf("Failed to join karma topic: %v", err)
		return
	}
	sub, err := topic.Subscribe()
	if err == nil {
		go r.subscribeLoop(ctx, sub)
	} else {
		glog.Errorf("Failed to subscribe to karma topic: %v", err)
	}

	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.propagateOnce(ctx, topic)
		}
	}
}

func (r *Reputer) subscribeLoop(ctx context.Context, sub *pubsub.Subscription) {
	for {
		msg, err := sub.Next(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			glog.Errorf("Subscription error on karma topic: %v", err)
			continue
		}
		if msg.ReceivedFrom == r.host.PeerID() {
			continue // skip our own messages
		}

		var remoteScores []*Score
		if err := json.Unmarshal(msg.Data, &remoteScores); err != nil {
			glog.V(4).Infof("Failed to unmarshal remote karma scores: %v", err)
			continue
		}
		
		r.MergeScores(msg.ReceivedFrom, remoteScores)
	}
}

// propagateOnce shares karma scores with connected peers
func (r *Reputer) propagateOnce(ctx context.Context, topic *pubsub.Topic) {
	r.mu.RLock()
	scores := make([]*Score, 0, len(r.localScores))
	for _, s := range r.localScores {
		scores = append(scores, s)
	}
	r.mu.RUnlock()

	data, err := json.Marshal(scores)
	if err != nil {
		glog.Errorf("Failed to marshal karma scores: %v", err)
		return
	}

	if err := topic.Publish(ctx, data); err != nil {
		glog.Errorf("Failed to publish karma scores: %v", err)
		return
	}

	glog.V(4).Infof("Propagated %d karma scores", len(scores))
}

// MergeScores merges karma scores from another peer
func (r *Reputer) MergeScores(peerID peer.ID, remoteScores []*Score) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, remote := range remoteScores {
		local, ok := r.localScores[remote.PeerID]
		if !ok {
			// New peer - add directly
			r.localScores[remote.PeerID] = remote
			continue
		}

		// Merge: keep the higher score
		if remote.Karma > local.Karma {
			r.localScores[remote.PeerID] = remote
		}
	}
}

// Save persists karma scores to disk
func (r *Reputer) Save() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	data, err := json.MarshalIndent(r.localScores, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal karma scores: %w", err)
	}

	return os.WriteFile(r.persistencePath, data, 0644)
}

// Load loads karma scores from disk
func (r *Reputer) Load() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	data, err := os.ReadFile(r.persistencePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // OK if file doesn't exist yet
		}
		return fmt.Errorf("failed to read karma scores: %w", err)
	}

	return json.Unmarshal(data, &r.localScores)
}
