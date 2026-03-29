// Package config handles configuration management for CompuShare nodes.
package config

import (
	"flag"
	"os"
	"path/filepath"
)

// Config holds all configuration for a CompuShare node
type Config struct {
	// DataDir is the directory for storing node data (keys, models, etc.)
	DataDir string

	// BootstrapPeers is a list of multiaddresses of bootstrap nodes
	BootstrapPeers []string

	// ListenAddresses are the multiaddresses to listen on
	ListenAddresses []string

	// ForceMode forces a specific node mode (consumer/provider/combined)
	ForceMode string

	// MaxConnections is the maximum number of peer connections
	MaxConnections int

	// EnableRelay enables circuit relay for NAT traversal
	EnableRelay bool

	// ModelDir is the directory containing cached model files
	ModelDir string

	// LogLevel controls logging verbosity
	LogLevel string
}

// HardwareInfo describes the hardware capabilities of a node
type HardwareInfo struct {
	// GPU model string (e.g., "NVIDIA RTX 4090", "AMD RX 7900 XT", "CPU-only")
	GPUModel string

	// VRAMGB is available GPU VRAM in GB
	VRAMGB int

	// RAMGB is available system RAM in GB
	RAMGB int

	// HasGPU indicates whether a GPU is available
	HasGPU bool

	// CUDAMajorVersion is the major CUDA version (0 if no NVIDIA GPU)
	CUDAMajorVersion int
}

// Parse parses command line flags and returns a Config
func Parse() *Config {
	cfg := &Config{}

	// Determine default data directory
	homeDir, _ := os.UserHomeDir()
	defaultDataDir := filepath.Join(homeDir, ".compushare")

	// Command line flags
	flag.StringVar(&cfg.DataDir, "data-dir", defaultDataDir, "Directory for node data")
	flag.StringVar(&cfg.ForceMode, "mode", "", "Force node mode: consumer, provider, or combined")
	flag.IntVar(&cfg.MaxConnections, "max-connections", 100, "Maximum number of peer connections")
	flag.BoolVar(&cfg.EnableRelay, "enable-relay", true, "Enable circuit relay for NAT traversal")
	flag.StringVar(&cfg.ModelDir, "model-dir", filepath.Join(defaultDataDir, "models"), "Directory for model files")
	flag.StringVar(&cfg.LogLevel, "log-level", "info", "Logging level: debug, info, warn, error")

	// Bootstrap peers (default to hardcoded network seeds)
	flag.Parse()

	// Set default bootstrap peers if none provided
	if len(cfg.BootstrapPeers) == 0 {
		cfg.BootstrapPeers = []string{
			"/ip4/104.131.0.69/tcp/4001/ipfs/QmSoLPppuBtQSGwKDZKrX73ksLevbALwHSCF5fxKRBzAfj",
			"/ip4/104.131.0.70/tcp/4001/ipfs/QmSoLju6m7WFbVWrTEtJ7uNY4cP6DFFN2PG5tCRJ9bbXL7",
		}
	}

	// Set default listen addresses
	if len(cfg.ListenAddresses) == 0 {
		cfg.ListenAddresses = []string{
			"/ip4/0.0.0.0/tcp/0",
			"/ip4/0.0.0.0/udp/0/quic",
		}
	}

	return cfg
}

// EnsureDataDir ensures the data directory exists with proper permissions
func (c *Config) EnsureDataDir() error {
	return os.MkdirAll(c.DataDir, 0700)
}
