// Package pipeline handles the orchestration of distributed inference
// across multiple provider nodes using pipeline parallelism.
package pipeline

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/compushare/compushare/crypto/he"
	"github.com/compushare/compushare/execution/llamacpp"
	"github.com/compushare/compushare/internal/config"
	"github.com/compushare/compushare/karma"
	"github.com/compushare/compushare/network/discovery"
	"github.com/compushare/compushare/network/libp2p"
	"github.com/compushare/compushare/network/tunnel"
	"github.com/compushare/compushare/storage"
	"github.com/compushare/compushare/verification"
	"github.com/golang/glog"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
)

// Orchestrator coordinates inference requests across multiple provider nodes.
// It handles provider selection, layer allocation, and result aggregation.
type Orchestrator struct {
	host     *libp2p.Host
	store    *storage.ContentStore
	reputer  *karma.Reputer
	caps     *discovery.CapabilityStore
	verifier *verification.Verifier
	cpStore     *CheckpointStore
	pendingReqs map[string]*InferenceRequest
	mu          sync.RWMutex
}

// NewOrchestrator creates a new orchestrator for consumer-side inference requests
func NewOrchestrator(host *libp2p.Host, store *storage.ContentStore, reputer *karma.Reputer) *Orchestrator {
	o := &Orchestrator{
		host:        host,
		store:       store,
		reputer:     reputer,
		caps:        discovery.NewCapabilityStore(),
		verifier:    verification.NewVerifier(host, reputer),
		cpStore:     NewCheckpointStore(),
		pendingReqs: make(map[string]*InferenceRequest),
	}
	o.verifier.SetReExecuter(o)
	return o
}

// Start starts the orchestrator — connects to network and begins capability subscription
func (o *Orchestrator) Start(ctx context.Context) error {
	glog.Info("Starting orchestrator...")

	// Start capability advertisement (consumer-only nodes send empty hardware info)
	hw := config.HardwareInfo{GPUModel: "CPU-only", RAMGB: 0, HasGPU: false}
	if err := discovery.StartCapabilityAdvertisement(ctx, o.host, hw, o.caps, nil); err != nil {
		return fmt.Errorf("failed to start capability advertisement: %w", err)
	}

	// Connect to bootstrap peers (best-effort)
	if err := o.host.ConnectToBootstrap(ctx); err != nil {
		glog.Warningf("Bootstrap connect failed (network may be empty): %v", err)
	}

	glog.Info("Orchestrator started")
	return nil
}

// InferenceRequest represents a request for distributed inference
type InferenceRequest struct {
	ModelID        string
	InputText      string
	MaxTokens      int
	Temperature    float64
	NumProviders   int
	PublicKey      *he.PublicKey
	SecretKey      *he.SecretKey // needed for decryption after pipeline
	EvaluationKeys *he.EvaluationKeys
	RequestID      string
	StreamCallback func(string) // Callback for streaming tokens directly to UI
}

// InferenceResult represents the result of a distributed inference request
type InferenceResult struct {
	RequestID       string
	OutputTokens    []int
	OutputText      string
	ProvidersUsed   []peer.ID
	ExecutionTimeMs int64
}

// RequestInference connects to providers via tunnels and runs the inference orchestrator.
func (o *Orchestrator) RequestInference(ctx context.Context, req *InferenceRequest) (*InferenceResult, error) {
	startTime := time.Now()

	// Step 1: Find available providers
	providers := o.findBestProviders(req.NumProviders)
	if len(providers) == 0 {
		return nil, fmt.Errorf("no providers available on the network")
	}
	glog.Infof("[%s] Selected %d providers", req.RequestID, len(providers))

	// Step 2: Allocate model layers across providers
	allocator := NewLayerAllocator(o.store)
	alloc := allocator.AllocateLayers(providers, req.ModelID)

	// Step 3: Open port-forward tunnels to each chosen provider
	var rpcTargets []llamacpp.RPCTarget
	var activeProviders []peer.ID

	for _, stage := range alloc.Stages {
		if stage.ProviderID == "" {
			continue
		}

		// Connect to the provider at a basic level first
		if err := o.connectToProvider(ctx, stage.ProviderID); err != nil {
			glog.Warningf("Failed to connect to provider %s: %v", stage.ProviderID, err)
			continue
		}

		// Open network tunnel to their sidecar port
		localPort, err := tunnel.OpenTunnel(ctx, o.host, stage.ProviderID.String())
		if err != nil {
			glog.Warningf("Failed to open tunnel to %s: %v", stage.ProviderID, err)
			continue
		}

		activeProviders = append(activeProviders, stage.ProviderID)
		rpcTargets = append(rpcTargets, llamacpp.RPCTarget{
			Host:       "127.0.0.1",
			Port:       localPort,
			PeerID:     stage.ProviderID.String(),
			StartLayer: stage.StartLayer,
			EndLayer:   stage.EndLayer,
		})
	}

	if len(rpcTargets) == 0 {
		return nil, fmt.Errorf("failed to open any tunnels to providers")
	}

	// Step 4: Resolve local model path
	modelPath, err := o.store.GetModelPath(req.ModelID)
	if err != nil || modelPath == "" {
		// Assuming Phase 1 requires consumer to have the GGUF model too, or we pass a mock.
		glog.Warningf("Model path not explicitly found, defaulting to model ID as path")
		modelPath = req.ModelID
	}

	// Step 5: Run distributed llama.cpp inference
	master := llamacpp.NewMaster(o.store.GetDataDir()) // Uses DataDir for bin path
	err = master.Run(ctx, modelPath, req.InputText, rpcTargets, req.StreamCallback)
	if err != nil {
		return nil, fmt.Errorf("master inference failed: %w", err)
	}

	execTime := time.Since(startTime).Milliseconds()

	// Step 6: Update karma for participating providers
	if len(activeProviders) > 0 {
		timePerProvider := execTime / int64(len(activeProviders))
		for _, pid := range activeProviders {
			o.reputer.AddContribution(pid, timePerProvider)
		}
	}

	result := &InferenceResult{
		RequestID:       req.RequestID,
		ProvidersUsed:   activeProviders,
		ExecutionTimeMs: execTime,
	}

	o.mu.Lock()
	o.pendingReqs[req.RequestID] = req
	o.mu.Unlock()

	return result, nil
}

// ReRequestInference implements the verification.ReExecuter interface for statistical sampling.
func (o *Orchestrator) ReRequestInference(ctx context.Context, requestID string) (*verification.ConsensusResult, error) {
	o.mu.RLock()
	req, ok := o.pendingReqs[requestID]
	o.mu.RUnlock()
	
	if !ok {
		return nil, fmt.Errorf("request %s not found for re-execution", requestID)
	}

	// Just execute the pipeline again with a completely new request ID to avoid endless loops
	newReq := *req
	newReq.RequestID = fmt.Sprintf("%s-reexec-%d", req.RequestID, time.Now().UnixNano())
	
	// Temporarily disable verifier loop by skipping verifier check inside RequestInference?
	// The problem is RequestInference automatically does VerifyResults which might trigger
	// another ReRequestInference! But VerifyResults has a statistical 5% chance. It's fine for MVP.
	
	// 1. Encrypt input
	encryptedInput, err := he.EncryptInput(newReq.InputText, newReq.PublicKey)
	if err != nil {
		return nil, err
	}
	
	providers := o.findBestProviders(newReq.NumProviders)
	allocator := NewLayerAllocator(o.store)
	alloc := allocator.AllocateLayers(providers, newReq.ModelID)
	
	encryptedOutput, providerIDs, err := o.executePipeline(ctx, &newReq, encryptedInput, alloc)
	if err != nil {
		return nil, err
	}
	
	outputBytes, err := he.DecryptOutput(encryptedOutput, newReq.SecretKey)
	if err != nil {
		return nil, err
	}
	
	outputTokens := make([]int, len(outputBytes))
	for i, b := range outputBytes {
		outputTokens[i] = int(b)
	}
	
	return &verification.ConsensusResult{
		RequestID: requestID, // original
		ConsensusTokens: outputTokens,
		ProvidersUsed: providerIDs,
		ConsensusAchieved: true,
	}, nil
}

// findBestProviders selects the best available providers for inference
func (o *Orchestrator) findBestProviders(count int) []*discovery.CapabilityManifest {
	all := o.caps.GetProviders()
	var valid []*discovery.CapabilityManifest
	for _, p := range all {
		if p.IsProvider && (p.VRAMGB > 0 || p.RAMGB >= 8) {
			valid = append(valid, p)
		}
	}
	// Sort by karma descending
	sort.Slice(valid, func(i, j int) bool {
		peerA, err := peer.Decode(valid[i].PeerID)
		if err != nil {
			return false
		}
		peerB, err := peer.Decode(valid[j].PeerID)
		if err != nil {
			return true
		}
		return o.reputer.GetScore(peerA).Karma > o.reputer.GetScore(peerB).Karma
	})

	if len(valid) > count {
		return valid[:count]
	}
	return valid
}

// executePipeline runs inference stages sequentially across providers.
// Encrypted activations flow from one provider to the next.
func (o *Orchestrator) executePipeline(
	ctx context.Context,
	req *InferenceRequest,
	encryptedInput []byte,
	alloc *LayerAllocation,
) ([]byte, []peer.ID, error) {
	if len(alloc.Stages) == 0 {
		return nil, nil, fmt.Errorf("no pipeline stages allocated")
	}

	activations := encryptedInput
	providerIDs := make([]peer.ID, 0, len(alloc.Stages))

	startIndex := 0
	if cp, ok := o.cpStore.LoadCheckpoint(req.RequestID); ok {
		for i, stage := range alloc.Stages {
			if stage.EndLayer == cp.EndLayer {
				startIndex = i + 1
				activations = cp.EncryptedState
				glog.Infof("[%s] Resuming from checkpoint after layer %d", req.RequestID, cp.EndLayer)
				break
			}
		}
	}

	for i := startIndex; i < len(alloc.Stages); i++ {
		stage := alloc.Stages[i]
		if stage.ProviderID == "" {
			glog.Warningf("Stage %d has empty provider ID, skipping", i)
			continue
		}

		glog.Infof("[%s] Stage %d/%d: layers %d-%d → provider %s",
			req.RequestID, i+1, len(alloc.Stages),
			stage.StartLayer, stage.EndLayer,
			stage.ProviderID.String()[:12]+"...")

		// Connect to provider (no-op if already connected)
		if err := o.connectToProvider(ctx, stage.ProviderID); err != nil {
			return nil, providerIDs, fmt.Errorf("stage %d: connect failed: %w", i, err)
		}

		// Send encrypted activations, receive next encrypted activations
		// Retry mechanism for provider failures
		maxRetries := 3
		var next []byte
		var err error
		for attempt := 0; attempt < maxRetries; attempt++ {
			next, err = o.executeStage(ctx, stage, req, activations)
			if err == nil {
				break
			}
			glog.Warningf("[%s] Stage %d failed, retry %d/%d: %v", req.RequestID, i, attempt+1, maxRetries, err)
			time.Sleep(time.Duration(attempt+1) * time.Second) // backoff
		}

		if err != nil {
			return nil, providerIDs, fmt.Errorf("stage %d failed after retries: %w", i, err)
		}

		activations = next
		providerIDs = append(providerIDs, stage.ProviderID)

		cp := NewCheckpoint(req.RequestID, req.ModelID, stage.StartLayer, stage.EndLayer, activations, stage.ProviderID)
		o.cpStore.SaveCheckpoint(req.RequestID, cp)
	}

	o.cpStore.DeleteCheckpoint(req.RequestID)

	return activations, providerIDs, nil
}

// connectToProvider ensures we have an active connection to a provider
func (o *Orchestrator) connectToProvider(ctx context.Context, providerID peer.ID) error {
	underlying := o.host.Underlying()

	// Already connected?
	if underlying.Network().Connectedness(providerID) == network.Connected {
		return nil
	}

	// Try peerstore addresses first
	addrs := underlying.Peerstore().Addrs(providerID)
	if len(addrs) > 0 {
		return underlying.Connect(ctx, peer.AddrInfo{ID: providerID, Addrs: addrs})
	}

	return fmt.Errorf("no addresses known for provider %s", providerID)
}

// executeStage sends encrypted activations to a provider and gets the next stage's output
func (o *Orchestrator) executeStage(
	ctx context.Context,
	stage PipelineStage,
	req *InferenceRequest,
	input []byte,
) ([]byte, error) {
	stream, err := o.host.Underlying().NewStream(ctx, stage.ProviderID,
		protocol.ID("/compushare/inference/1.0"))
	if err != nil {
		return nil, fmt.Errorf("failed to open stream: %w", err)
	}
	defer stream.Close()

	// 1. Prepare and write stage request as a single JSON object
	// Serialize evaluation keys
	evalKeysBytes, err := req.EvaluationKeys.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("failed to marshal evaluation keys: %w", err)
	}

	requestPayload := map[string]interface{}{
		"model_id":     req.ModelID,
		"start_layer":  stage.StartLayer,
		"end_layer":    stage.EndLayer,
		"encrypted_in": base64.StdEncoding.EncodeToString(input),
		"eval_keys":    base64.StdEncoding.EncodeToString(evalKeysBytes),
	}
	requestData, err := json.Marshal(requestPayload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}
	if _, err := stream.Write(requestData); err != nil {
		return nil, fmt.Errorf("failed to write request to stream: %w", err)
	}

	// 3. Read encrypted output activations — limit to 64 MB (max expected activation size)
	const maxResponseBytes = 64 << 20 // 64 MB
	output, err := io.ReadAll(io.LimitReader(stream, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to read output from stream: %w", err)
	}

	return output, nil
}

// decodeTokens converts token IDs to text using simple byte mapping
func decodeTokens(tokens []int) string {
	var sb strings.Builder
	for _, t := range tokens {
		if t >= 32 && t < 127 { // printable ASCII
			sb.WriteByte(byte(t))
		}
	}
	result := sb.String()
	if result == "" {
		return fmt.Sprintf("[%d tokens]", len(tokens))
	}
	return result
}
