package llamacpp

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/golang/glog"
)

// RPCTarget represents a remote node that processes a specific range of model layers.
type RPCTarget struct {
	Host       string // Localhost tunnel IP
	Port       int    // Localhost tunnel Port
	PeerID     string // Network ID (for UI mapping)
	StartLayer int
	EndLayer   int
}

// Master manages the llama-cli orchestration process.
type Master struct {
	binaryPath string
}

// NewMaster creates a new master orchestrator instance.
func NewMaster(dataDir string) *Master {
	binName := "llama-cli"
	if os.PathSeparator == '\\' {
		binName = "llama-cli.exe"
	}
	return &Master{
		binaryPath: filepath.Join(dataDir, "bin", binName),
	}
}

// EnsureBinary downloads or ensures the llama-cli binary exists.
// Same logic conceptually as DownloadBinary for sidecar, simplified for MVP.
func (m *Master) EnsureBinary() error {
	if _, err := os.Stat(m.binaryPath); err == nil {
		return nil
	}

	// For MVP, we presume the user has downloaded llama-cli if they are acting as the consumer
	// or we would use DownloadBinary logic here extending the github asset fetcher.
	// We'll instruct users to place it in the bin folder if it auto-download fails.
	glog.Warningf("llama-cli binary not found at %s. Please ensure it's installed for consumer mode.", m.binaryPath)
	return nil
}

// Run executes the inference prompt distributed across the target RPC nodes.
func (m *Master) Run(ctx context.Context, modelPath, prompt string, targets []RPCTarget, tokenCallback func(string)) error {
	if err := m.EnsureBinary(); err != nil {
		return err
	}

	// Build the RPC argument string (format: host:port,host:port)
	var rpcServers []string
	for _, target := range targets {
		rpcServers = append(rpcServers, fmt.Sprintf("%s:%d", target.Host, target.Port))
	}
	rpcFlag := strings.Join(rpcServers, ",")

	args := []string{
		"-m", modelPath,
		"-p", prompt,
		"-n", "512", // Generate up to 512 tokens
		"--rpc", rpcFlag,
		// Split logic varies by llama.cpp version; traditionally it auto-distributes 
		// if multiple --rpc targets are given and tensor-split / rpc usage is configured.
		// For the MVP, we let llama-cli handle the default layer split among specified targets.
		"--threads", "4",
		"--ctx-size", "2048",
	}

	glog.Infof("Executing distributed inference: %s %v", m.binaryPath, args)
	cmd := exec.CommandContext(ctx, m.binaryPath, args...)

	// Connect stdout to read streaming tokens
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to open stdout pipe: %w", err)
	}
	
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start llama-cli master: %w", err)
	}

	// Read output continuously
	scanner := bufio.NewScanner(stdoutPipe)
	// Some output might not have newlines per token, so we split by spaces/words if needed,
	// but llama-cli normally flushes stdout per token or word
	scanner.Split(bufio.ScanRunes)

	var outputBuffer strings.Builder
	for scanner.Scan() {
		text := scanner.Text()
		outputBuffer.WriteString(text)
		
		// Send non-empty chunks back to the UI
		if tokenCallback != nil {
			tokenCallback(text)
		}
	}

	if err := scanner.Err(); err != nil {
		glog.Errorf("Error reading output state: %v", err)
	}

	// Wait for process completion
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err() // Killed via context
		}
		return fmt.Errorf("master process returned error: %w", err)
	}

	return nil
}
