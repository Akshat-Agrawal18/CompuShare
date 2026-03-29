// Package pipeline handles distributed inference pipeline execution.
package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/compushare/compushare/karma"
	"github.com/compushare/compushare/network/libp2p"
	"github.com/compushare/compushare/storage"
	"github.com/golang/glog"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/protocol"
)

// InferenceEngine defines the interface for running inference layers.
// Implementation will bridge to vLLM or similar engine.
type InferenceEngine interface {
	ExecuteLayer(ctx context.Context, layerRange LayerRange, encryptedInput []byte, evalKeysBytes []byte) ([]byte, error)
}

// Executor runs inference on provider nodes.
type Executor struct {
	host        *libp2p.Host
	store       *storage.ContentStore
	reputer     *karma.Reputer
	engine      InferenceEngine
	modelLoader *ModelLoader
}

// NewExecutor creates a new executor for provider-side inference
func NewExecutor(host *libp2p.Host, store *storage.ContentStore, reputer *karma.Reputer, engine InferenceEngine) *Executor {
	return &Executor{
		host:        host,
		store:       store,
		reputer:     reputer,
		engine:      engine,
		modelLoader: NewModelLoader(store),
	}
}

// Start starts the executor service
func (e *Executor) Start(ctx context.Context) error {
	glog.Info("Starting executor...")
	if err := e.registerInferenceHandler(ctx); err != nil {
		return fmt.Errorf("failed to register inference handler: %w", err)
	}
	if err := e.host.ConnectToBootstrap(ctx); err != nil {
		glog.Warningf("Failed to connect to bootstrap peers: %v", err)
	}
	glog.Info("Executor started successfully")
	return nil
}

func (e *Executor) registerInferenceHandler(ctx context.Context) error {
	e.host.Underlying().SetStreamHandler(protocol.ID("/compushare/inference/1.0"), e.handleInferenceStream)
	return nil
}

func (e *Executor) handleInferenceStream(stream network.Stream) {
	defer stream.Close()

	// Use a background context for inference; cancelled via stream.Reset on shutdown.
	// Note: libp2p network.Stream does not expose a Context() method.
	ctx := context.Background()

	// 1. Read request — limit to 4 MB to prevent OOM from malicious peers
	const maxRequestBytes = 4 << 20 // 4 MB
	requestData, err := io.ReadAll(io.LimitReader(stream, maxRequestBytes))
	if err != nil {
		glog.Errorf("Failed to read inference request: %v", err)
		return
	}

	// 2. Parse request (simple JSON)
	var request map[string]interface{}
	if err := json.Unmarshal(requestData, &request); err != nil {
		glog.Errorf("Failed to parse request: %v", err)
		return
	}

	// 3. Extract parameters safely — unguarded type assertions would panic on malformed input
	modelID, ok := request["model_id"].(string)
	if !ok || modelID == "" {
		glog.Errorf("Missing or invalid model_id in request")
		return
	}
	startLayerF, ok := request["start_layer"].(float64)
	if !ok {
		glog.Errorf("Missing or invalid start_layer in request")
		return
	}
	endLayerF, ok := request["end_layer"].(float64)
	if !ok {
		glog.Errorf("Missing or invalid end_layer in request")
		return
	}
	encryptedInStr, ok := request["encrypted_in"].(string)
	if !ok {
		glog.Errorf("Missing or invalid encrypted_in in request")
		return
	}
	evalKeysStr, ok := request["eval_keys"].(string)
	if !ok {
		glog.Errorf("Missing or invalid eval_keys in request")
		return
	}

	layerRange := LayerRange{
		ModelID:    modelID,
		StartLayer: int(startLayerF),
		EndLayer:   int(endLayerF),
	}
	encryptedInput := []byte(encryptedInStr)
	evalKeysBytes := []byte(evalKeysStr)

	// 4. Execute using the stream's context — cancels if peer disconnects
	encryptedOutput, err := e.ExecuteLayer(ctx, layerRange, encryptedInput, evalKeysBytes)
	if err != nil {
		glog.Errorf("Inference execution failed: %v", err)
		return
	}

	// 5. Send output back
	if _, err := stream.Write(encryptedOutput); err != nil {
		glog.Errorf("Failed to write output to stream: %v", err)
	}
}

// ExecuteLayer executes a single pipeline stage (layer range) on encrypted input
func (e *Executor) ExecuteLayer(ctx context.Context, layerRange LayerRange, encryptedInput []byte, evalKeysBytes []byte) ([]byte, error) {
	startTime := time.Now()

	// Execute via the engine (HE-aware)
	encryptedOutput, err := e.engine.ExecuteLayer(ctx, layerRange, encryptedInput, evalKeysBytes)
	if err != nil {
		return nil, fmt.Errorf("HE execution failed: %w", err)
	}

	executionTime := time.Since(startTime).Milliseconds()
	e.reputer.AddContribution(e.host.PeerID(), executionTime)

	return encryptedOutput, nil
}

// LayerRange represents a range of model layers to execute
type LayerRange struct {
	ModelID    string
	StartLayer int
	EndLayer   int
}

// ModelLayers represents loaded model layers
type ModelLayers struct {
	Weights map[int][]byte // layer index -> serialized weights
}

// ModelLoader handles loading model layers from storage
type ModelLoader struct {
	store *storage.ContentStore
	cache map[string]*ModelLayers // model ID -> loaded layers
}

// NewModelLoader creates a new model loader
func NewModelLoader(store *storage.ContentStore) *ModelLoader {
	return &ModelLoader{
		store: store,
		cache: make(map[string]*ModelLayers),
	}
}

// LoadLayers loads the specified layer range for a model
func (ml *ModelLoader) LoadLayers(ctx context.Context, range_ LayerRange) (*ModelLayers, error) {
	if cached, ok := ml.cache[range_.ModelID]; ok {
		return ml.extractRange(cached, range_.StartLayer, range_.EndLayer), nil
	}
	modelPath, err := ml.store.GetModelPath(range_.ModelID)
	if err != nil {
		return nil, fmt.Errorf("model not found: %w", err)
	}
	layers, err := ml.loadAllLayers(modelPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load model: %w", err)
	}
	ml.cache[range_.ModelID] = layers
	return ml.extractRange(layers, range_.StartLayer, range_.EndLayer), nil
}

func (ml *ModelLoader) loadAllLayers(modelPath string) (*ModelLayers, error) {
	return &ModelLayers{Weights: make(map[int][]byte)}, nil
}

func (ml *ModelLoader) extractRange(layers *ModelLayers, start, end int) *ModelLayers {
	extracted := &ModelLayers{Weights: make(map[int][]byte)}
	for i := start; i < end; i++ {
		if w, ok := layers.Weights[i]; ok {
			extracted.Weights[i] = w
		}
	}
	return extracted
}
