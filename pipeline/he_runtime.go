// Package pipeline handles the orchestration of distributed inference.
package pipeline

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/golang/glog"
)

// defaultInferenceTimeout is the default timeout for vLLM inference requests.
const defaultInferenceTimeout = 120 * time.Second

// vLLMResponse is the expected JSON response from the vLLM /execute endpoint.
type vLLMResponse struct {
	EncryptedOut string `json:"encrypted_out"`
	TokensUsed   int    `json:"tokens_used,omitempty"`
	Error        string `json:"error,omitempty"`
}

// VLLMEngine implements InferenceEngine by delegating to a local vLLM HTTP instance.
type VLLMEngine struct {
	ModelPath string
	Endpoint  string // base URL of the running vLLM instance
	client    *http.Client
}

// NewVLLMEngine creates a new inference engine backed by a vLLM server.
func NewVLLMEngine(modelPath, endpoint string) *VLLMEngine {
	return &VLLMEngine{
		ModelPath: modelPath,
		Endpoint:  endpoint,
		client:    &http.Client{Timeout: defaultInferenceTimeout},
	}
}

// HealthCheck verifies that the vLLM server is running and ready.
// It sends a GET request to {endpoint}/health and returns nil on 200.
func (v *VLLMEngine) HealthCheck(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "GET", v.Endpoint+"/health", nil)
	if err != nil {
		return fmt.Errorf("failed to create health check request: %w", err)
	}

	resp, err := v.client.Do(req)
	if err != nil {
		return fmt.Errorf("vLLM health check failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("vLLM health check returned status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// ExecuteLayer implements InferenceEngine.
func (v *VLLMEngine) ExecuteLayer(ctx context.Context, layerRange LayerRange, encryptedInput []byte, evalKeysBytes []byte) ([]byte, error) {
	glog.Infof("Executing layers %d-%d on model %s",
		layerRange.StartLayer, layerRange.EndLayer, layerRange.ModelID)

	// Prepare request payload — base64-encode binary data for reliable JSON transport
	payload := map[string]interface{}{
		"model_id":     layerRange.ModelID,
		"start_layer":  layerRange.StartLayer,
		"end_layer":    layerRange.EndLayer,
		"encrypted_in": base64.StdEncoding.EncodeToString(encryptedInput),
	}
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal payload: %w", err)
	}

	// Call vLLM instance
	req, err := http.NewRequestWithContext(ctx, "POST", v.Endpoint+"/execute", bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := v.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to call vLLM: %w", err)
	}
	defer resp.Body.Close()

	// Read the full body once — we may need it for either success or error
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read vLLM response body: %w", err)
	}

	// Non-200 responses: try to extract a structured error message
	if resp.StatusCode != http.StatusOK {
		var errResp vLLMResponse
		if json.Unmarshal(body, &errResp) == nil && errResp.Error != "" {
			return nil, fmt.Errorf("vLLM error (HTTP %d): %s", resp.StatusCode, errResp.Error)
		}
		return nil, fmt.Errorf("vLLM returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	// Decode successful response
	var result vLLMResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to decode vLLM response: %w", err)
	}
	if result.Error != "" {
		return nil, fmt.Errorf("vLLM returned error in response body: %s", result.Error)
	}

	// Base64-decode the encrypted output
	decoded, err := base64.StdEncoding.DecodeString(result.EncryptedOut)
	if err != nil {
		return nil, fmt.Errorf("failed to base64-decode encrypted_out: %w", err)
	}

	return decoded, nil
}
