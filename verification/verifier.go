// Package verification handles result verification and provider challenge mechanisms.
// Implements multi-provider consensus and statistical sampling for trust without TEE.
package verification

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/compushare/compushare/karma"
	"github.com/compushare/compushare/network/libp2p"
	"github.com/golang/glog"
	"github.com/libp2p/go-libp2p/core/peer"
)

// Verifier coordinates result verification across multiple providers
type Verifier struct {
	host    *libp2p.Host
	reputer *karma.Reputer

	// reExecuter interface allows the orchestrator to re-run inference
	reExecuter ReExecuter

	// challengeRate is the percentage of results to randomly verify (0.05 = 5%)
	challengeRate float64

	// pendingChallenges tracks in-progress verifications
	pendingChallenges map[string]*Challenge

	mu sync.Mutex
}

// ReExecuter defines the interface for re-running inference for verification.
type ReExecuter interface {
	ReRequestInference(ctx context.Context, requestID string) (*ConsensusResult, error)
}

// Challenge represents an in-progress verification challenge
type Challenge struct {
	RequestID      string
	OriginalResult []byte
	Providers      []peer.ID
	StartedAt      time.Time
	Verified       bool
}

// NewVerifier creates a new verifier
func NewVerifier(host *libp2p.Host, reputer *karma.Reputer) *Verifier {
	return &Verifier{
		host:              host,
		reputer:           reputer,
		challengeRate:     0.05, // 5% of results are verified
		pendingChallenges: make(map[string]*Challenge),
	}
}

// SetReExecuter sets the re-executer for testing and injection
func (v *Verifier) SetReExecuter(r ReExecuter) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.reExecuter = r
}

// Result represents a single provider's inference result
type Result struct {
	ProviderID    peer.ID
	Tokens        []int
	ResultHash    string // SHA-256 hash of tokens for comparison
	ExecutionTime int64  // milliseconds
}

// ConsensusResult represents the consensus result from multiple providers
type ConsensusResult struct {
	RequestID          string
	ConsensusTokens    []int
	ProvidersUsed      []peer.ID
	DisagreedProviders []peer.ID // Providers who disagreed with consensus
	ConsensusAchieved  bool
}

// VerifyResults takes results from multiple providers and determines consensus
func (v *Verifier) VerifyResults(requestID string, results []*Result, threshold int) (*ConsensusResult, error) {
	if len(results) < threshold {
		return nil, fmt.Errorf("insufficient results: have %d, need %d", len(results), threshold)
	}

	// Group results by their hash (identical results will have identical hashes)
	hashGroups := make(map[string][]*Result)
	for _, r := range results {
		hashGroups[r.ResultHash] = append(hashGroups[r.ResultHash], r)
	}

	// Find the majority hash
	var majorityHash string
	var maxCount int
	for hash, group := range hashGroups {
		if len(group) > maxCount {
			maxCount = len(group)
			majorityHash = hash
		}
	}

	// Check if consensus was achieved
	consensusAchieved := maxCount >= threshold

	// Find disagreed providers (those not in the majority group)
	disagreedProviders := make([]peer.ID, 0)
	for _, r := range results {
		if r.ResultHash != majorityHash {
			disagreedProviders = append(disagreedProviders, r.ProviderID)
		}
	}

	// Get the consensus tokens
	consensusTokens := hashGroups[majorityHash][0].Tokens

	// Penalize disagreed providers
	for _, providerID := range disagreedProviders {
		v.reputer.AddFailure(providerID)
		glog.Warningf("Provider %s disagreed with consensus (hash mismatch)", providerID)
	}

	// Update karma for providers that agreed
	for _, r := range results {
		if r.ResultHash == majorityHash {
			v.reputer.AddContribution(r.ProviderID, r.ExecutionTime)
		}
	}

	// Record this verification for statistical sampling
	v.recordVerification(requestID, results, consensusTokens)

	return &ConsensusResult{
		RequestID:          requestID,
		ConsensusTokens:    consensusTokens,
		ProvidersUsed:      extractProviderIDs(results),
		DisagreedProviders: disagreedProviders,
		ConsensusAchieved:  consensusAchieved,
	}, nil
}

// recordVerification records a verification for potential later statistical sampling
func (v *Verifier) recordVerification(requestID string, results []*Result, consensusTokens []int) {
	v.mu.Lock()
	defer v.mu.Unlock()

	// Compute hash of consensus tokens for later comparison
	h := sha256.New()
	for _, t := range consensusTokens {
		h.Write([]byte(fmt.Sprintf("%d-", t)))
	}
	consensusHash := hex.EncodeToString(h.Sum(nil))

	challenge := &Challenge{
		RequestID:      requestID,
		OriginalResult: []byte(consensusHash),
		Providers:      make([]peer.ID, len(results)),
		StartedAt:      time.Now(),
		Verified:       true,
	}
	for i, r := range results {
		challenge.Providers[i] = r.ProviderID
	}

	v.pendingChallenges[requestID] = challenge

	// Randomly select some for re-verification
	if rand.Float64() < v.challengeRate {
		go v.runStatisticalVerification(requestID, results, consensusTokens)
	}
}

// runStatisticalVerification re-runs inference to verify a randomly selected result
func (v *Verifier) runStatisticalVerification(requestID string, results []*Result, consensusTokens []int) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	glog.V(3).Infof("Running statistical verification for request %s", requestID)

	if v.reExecuter == nil {
		glog.Warningf("Statistical verification skipped for %s: no ReExecuter configured", requestID)
		return
	}

	// Re-execute the inference
	reResult, err := v.reExecuter.ReRequestInference(ctx, requestID)
	if err != nil {
		glog.Warningf("Statistical verification failed for %s: re-execution error: %v", requestID, err)
		return
	}

	// Hash the re-executed consensus tokens for comparison
	h := sha256.New()
	for _, t := range reResult.ConsensusTokens {
		h.Write([]byte(fmt.Sprintf("%d-", t)))
	}
	reHash := hex.EncodeToString(h.Sum(nil))

	// Compute hash of the original consensus tokens
	hOrig := sha256.New()
	for _, t := range consensusTokens {
		hOrig.Write([]byte(fmt.Sprintf("%d-", t)))
	}
	origHash := hex.EncodeToString(hOrig.Sum(nil))

	// Compare the hashes
	if reHash != origHash {
		glog.Warningf("Statistical verification FAILED for %s: hash mismatch (re-run=%s, original=%s)",
			requestID, reHash, origHash)
		// Mark original providers as disagreeing and apply karma penalties
		for _, providerID := range extractProviderIDs(results) {
			v.reputer.AddFailure(providerID)
			glog.Warningf("Provider %s penalized after statistical re-verification failure", providerID)
		}
	} else {
		glog.V(3).Infof("Statistical verification PASSED for %s", requestID)
	}

	// Store verification result for audit
	v.mu.Lock()
	if challenge, ok := v.pendingChallenges[requestID]; ok {
		challenge.Verified = (reHash == origHash)
	}
	v.mu.Unlock()

	glog.V(3).Infof("Statistical verification complete for %s", requestID)
}

// CompareEncryptedOutputs compares two encrypted outputs for equality
// Since we can't decrypt, we compare encrypted representations
func (v *Verifier) CompareEncryptedOutputs(a, b []byte) (bool, error) {
	// Compare as raw bytes (not meaningful for HE but used for testing)
	if len(a) != len(b) {
		return false, nil
	}

	// Compute and compare hashes
	ha := sha256.Sum256(a)
	hb := sha256.Sum256(b)

	for i := range ha {
		if ha[i] != hb[i] {
			return false, nil
		}
	}
	return true, nil
}

// AddResult hashes the tokens for comparison
func AddResult(providerID peer.ID, tokens []int, execTime int64) *Result {
	h := sha256.New()
	for _, t := range tokens {
		h.Write([]byte(fmt.Sprintf("%d-", t)))
	}

	return &Result{
		ProviderID:    providerID,
		Tokens:        tokens,
		ResultHash:    hex.EncodeToString(h.Sum(nil)),
		ExecutionTime: execTime,
	}
}

// DeterministicHasher provides deterministic hashing for reproducibility verification
type DeterministicHasher struct {
	seed int64
}

// NewDeterministicHasher creates a hasher with a specific seed
// Using the same seed allows verification that inference was deterministic
func NewDeterministicHasher(seed int64) *DeterministicHasher {
	return &DeterministicHasher{seed: seed}
}

// HashResult computes a deterministic hash of the inference result
// This can be used to verify that the same input produced the same output
func (dh *DeterministicHasher) HashResult(tokens []int) string {
	h := sha256.New()
	h.Write([]byte(fmt.Sprintf("seed:%d:", dh.seed)))
	for i, t := range tokens {
		h.Write([]byte(fmt.Sprintf("%d:%d-", i, t)))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// VerifyDeterministic checks if a result matches the expected hash
func (dh *DeterministicHasher) VerifyDeterministic(tokens []int, expectedHash string) bool {
	actualHash := dh.HashResult(tokens)
	return actualHash == expectedHash
}

// MerkleCheckpoint represents a provider's commitment to model state
type MerkleCheckpoint struct {
	RequestID  string
	ModelID    string
	LayerRange [2]int // [start, end] layer indices
	RootHash   string // Merkle root of model weights used
	Timestamp  int64
	Signature  []byte
}

// CreateCheckpoint creates a Merkle checkpoint for provider's layer range
func CreateCheckpoint(requestID, modelID string, layerRange [2]int, weights map[int][]byte) *MerkleCheckpoint {
	// Compute Merkle tree of layer weights
	h := sha256.New()

	// Hash each layer's weights
	layerHashes := make([]string, 0, len(weights))
	for i := 0; i <= layerRange[1]-layerRange[0]; i++ {
		layerHash := sha256.Sum256(weights[i])
		layerHashes = append(layerHashes, hex.EncodeToString(layerHash[:]))
		h.Write(layerHash[:])
	}

	// Root hash of all layer hashes
	rootHash := hex.EncodeToString(h.Sum(nil))

	return &MerkleCheckpoint{
		RequestID:  requestID,
		ModelID:    modelID,
		LayerRange: layerRange,
		RootHash:   rootHash,
		Timestamp:  time.Now().Unix(),
	}
}

// VerifyCheckpoint verifies a provider's checkpoint by recomputing the Merkle root
// and checking that the signature field is present.
func (v *Verifier) VerifyCheckpoint(cp *MerkleCheckpoint, weights map[int][]byte) bool {
	if cp == nil {
		glog.Warning("VerifyCheckpoint: nil checkpoint")
		return false
	}

	// Verify signature is present and non-empty
	if len(cp.Signature) == 0 {
		glog.Warningf("VerifyCheckpoint: missing signature for request %s", cp.RequestID)
		return false
	}

	// Recompute the Merkle root hash from the provided layer weights
	h := sha256.New()
	for i := 0; i <= cp.LayerRange[1]-cp.LayerRange[0]; i++ {
		layerData, ok := weights[i]
		if !ok {
			glog.Warningf("VerifyCheckpoint: missing layer %d data for request %s", i, cp.RequestID)
			return false
		}
		layerHash := sha256.Sum256(layerData)
		h.Write(layerHash[:])
	}
	recomputedRoot := hex.EncodeToString(h.Sum(nil))

	if recomputedRoot != cp.RootHash {
		glog.Warningf("VerifyCheckpoint: root hash mismatch for request %s: got %s, want %s",
			cp.RequestID, recomputedRoot, cp.RootHash)
		return false
	}

	return true
}

// extractProviderIDs returns a slice of peer IDs from a result set
func extractProviderIDs(results []*Result) []peer.ID {
	ids := make([]peer.ID, len(results))
	for i, r := range results {
		ids[i] = r.ProviderID
	}
	return ids
}
