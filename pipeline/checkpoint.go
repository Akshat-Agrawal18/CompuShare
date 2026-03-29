// Package pipeline handles the orchestration of distributed inference
// across multiple provider nodes using pipeline parallelism.
package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

// EncryptedCheckpoint represents a snapshot of the pipeline state between stages.
type EncryptedCheckpoint struct {
	RequestID      string
	ModelID        string
	StartLayer     int
	EndLayer       int
	EncryptedState []byte // The encrypted activations
	ProviderID     peer.ID
	Timestamp      int64
	Signature      []byte // Signed by the provider
}

// NewCheckpoint creates a new checkpoint for a pipeline stage output.
func NewCheckpoint(requestID, modelID string, startLayer, endLayer int, state []byte, provider peer.ID) *EncryptedCheckpoint {
	return &EncryptedCheckpoint{
		RequestID:      requestID,
		ModelID:        modelID,
		StartLayer:     startLayer,
		EndLayer:       endLayer,
		EncryptedState: state,
		ProviderID:     provider,
		Timestamp:      time.Now().Unix(),
	}
}

// Hash returns a unique identifier for this checkpoint state.
func (c *EncryptedCheckpoint) Hash() string {
	h := sha256.New()
	h.Write([]byte(c.RequestID))
	h.Write([]byte(c.ModelID))
	h.Write(c.EncryptedState)
	return hex.EncodeToString(h.Sum(nil))
}

// VerifySignature verifies the provider's signature on the checkpoint.
func (c *EncryptedCheckpoint) VerifySignature() error {
	pubKey, err := c.ProviderID.ExtractPublicKey()
	if err != nil {
		return fmt.Errorf("failed to extract public key from peer ID: %w", err)
	}
	hash := c.Hash()
	ok, err := pubKey.Verify([]byte(hash), c.Signature)
	if err != nil {
		return fmt.Errorf("error verifying signature: %w", err)
	}
	if !ok {
		return fmt.Errorf("invalid signature")
	}
	return nil
}

// CheckpointStore manages storage and retrieval of EncryptedCheckpoints.
type CheckpointStore struct {
	checkpoints map[string]*EncryptedCheckpoint
	mu          sync.RWMutex
}

// NewCheckpointStore creates a new CheckpointStore.
func NewCheckpointStore() *CheckpointStore {
	return &CheckpointStore{
		checkpoints: make(map[string]*EncryptedCheckpoint),
	}
}

// SaveCheckpoint saves a checkpoint.
func (s *CheckpointStore) SaveCheckpoint(reqID string, cp *EncryptedCheckpoint) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checkpoints[reqID] = cp
}

// LoadCheckpoint retrieves a checkpoint.
func (s *CheckpointStore) LoadCheckpoint(reqID string) (*EncryptedCheckpoint, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cp, ok := s.checkpoints[reqID]
	return cp, ok
}

// DeleteCheckpoint removes a checkpoint.
func (s *CheckpointStore) DeleteCheckpoint(reqID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.checkpoints, reqID)
}
