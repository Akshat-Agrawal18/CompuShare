# CompuShare: P2P Distributed AI Compute Network

A torrenting-inspired P2P network where users share GPU/CPU compute for running AI models. Like BitTorrent, every node simultaneously consumes and provides resources — seeding compute in the background like seeding torrents.

## Core Idea

**Problem**: Running large AI models requires expensive GPU hardware that most people don't have access to.

**Solution**: A decentralized network where users with GPUs contribute idle compute to help others run AI models, and in turn can use the network to run models themselves.

**Key Features**:
- **Free to use** — no tokens, no credits, no payments
- **Privacy-preserving** — Homomorphic Encryption ensures your data never leaves your machine unencrypted
- **Pipeline parallelism** — models are split across multiple providers, not replicated
- **Karma system** — tracks contribution but doesn't gate access (like Reddit karma)
- **One-click activation** — download → install → auto-joins network

## How It Works

```
1. You download and install CompuShare
2. It auto-detects your hardware (GPU + RAM)
3. It joins the P2P network and starts "seeding" compute
4. When you want to run an AI model:
   - Your input is encrypted locally (Homomorphic Encryption)
   - Encrypted input is split across 3+ provider nodes
   - Each provider runs different layers (pipeline parallelism)
   - Encrypted results flow through the pipeline
   - Final encrypted output is returned to you
   - You decrypt it locally — no one ever sees your data
5. Your karma score increases based on compute contributed
```

## Architecture

### Node Model (Torrenting-Inspired)

Every node runs three roles simultaneously:

| Role | Description |
|------|-------------|
| **Consumer** | Request AI inference, encrypt/decrypt locally |
| **Provider** | Run inference stages for others (background) |
| **Seeder** | Share model chunks with the network (like torrent seeding) |

### Privacy Architecture

```
Consumer's Machine:
  Input: "explain quantum computing"
       ↓
  Local Homomorphic Encryption (HE) — consumer's key only
       ↓
  Encrypted input → Provider A (layers 0-15)
  Encrypted input → Provider B (layers 16-31)
  Encrypted input → Provider C (layers 32-47)

  Providers can ONLY compute on encrypted data
  They cannot decrypt, cannot see input or output
  Only the consumer can decrypt with their private key
```

### Hardware Resources

"Hardware" = GPU + RAM collectively:

- **GPU**: Compute-intensive operations (matrix multiplications)
- **RAM**: Model weight storage, KV cache, intermediate activations

Both are scarce resources that providers contribute to the network.

## Technology Stack

| Component | Technology |
|-----------|------------|
| P2P Networking | libp2p (Go) |
| Homomorphic Encryption | Microsoft SEAL (CKKS) |
| Model Distribution | BitTorrent-style chunking |
| Inference Runtime | vLLM |
| Identity | Ed25519 PeerID |
| Karma Storage | DHT-based (no blockchain) |

## Building

### Prerequisites

- Go 1.21+
- For GPU support: NVIDIA GPU with CUDA, or DirectML (Windows)
- 8GB+ RAM recommended

### Build

```bash
# Clone the repository
git clone https://github.com/compushare/compushare.git
cd compushare

# Build all binaries
go build -o bin/compushare ./cmd/compushare
go build -o bin/provider ./cmd/provider
go build -o bin/consumer ./cmd/consumer
```

### Running

**As a combined node (default)**:
```bash
./bin/compushare
```

**As a provider-only node**:
```bash
./bin/provider
```

**As a consumer-only node**:
```bash
./bin/consumer --interactive
```

**Consumer CLI options**:
```bash
./bin/consumer --model tinyllama-1b --max-tokens 256 "Hello, explain AI"
./bin/consumer --interactive
```

## Directory Structure

```
compushare/
├── cmd/
│   ├── compushare/main.go      # Single binary for all node types
│   ├── provider/main.go        # Provider service
│   └── consumer/main.go         # Consumer CLI
├── network/
│   ├── libp2p/host.go          # P2P networking foundation
│   └── discovery/advertiser.go  # GossipSub capability ads
├── crypto/
│   └── he/encryptor.go         # Homomorphic encryption
├── pipeline/
│   ├── orchestrator.go         # Consumer-side pipeline coordination
│   └── executor.go             # Provider-side inference execution
├── storage/
│   └── content_store.go        # Content-addressed chunk storage
├── model/
│   └── torrent/piece_manager.go # BitTorrent-style model distribution
├── karma/
│   └── reputer.go              # Karma score tracking
├── verification/
│   └── verifier.go             # Multi-provider consensus
├── internal/
│   └── config/config.go        # Configuration
└── docs/
    └── protocol.md              # Protocol specification
```

## Safety & Security

### Privacy (Homomorphic Encryption)
- Consumer encrypts input locally before sending
- Providers compute on encrypted data only
- Encrypted activations flow between pipeline stages
- Only consumer can decrypt final output
- FHE scheme: CKKS (approximate arithmetic for ML)

### Sybil Resistance
- Each node = one PeerID (cryptographic identity)
- Karma requires sustained contribution
- No instant fake karma possible

### Result Integrity
- Pipeline stages run sequentially
- Provider dropout = checkpoint + restart
- Statistical sampling: 5% of pipelines re-verified
- Karma penalty for failures

### DoS Prevention
- GossipSub peer scoring
- Per-peer rate limiting
- Circuit relay quotas

## Karma System

Karma is a contribution score visible to all, but **does NOT affect priority** — everyone gets equal access.

```
Karma = 1.0 + log(total_compute_contributed_ms)
```

- **New users** start at karma = 1.0
- **Providers** earn karma for each layer they process
- **Consumers** karma increases proportional to their contribution ratio
- **Failures** reduce karma by 5%

Karma is stored in the DHT and propagates across the network.

## Status

**Current Phase**: Phase 1 MVP

This is an early-stage project. The architecture is designed but implementation is in progress.

### TODO (Phase 1 MVP)

- [x] Project structure
- [x] P2P networking (libp2p)
- [x] Capability discovery
- [x] Karma system
- [x] Consumer CLI
- [x] Provider service
- [x] Pipeline orchestrator (design)
- [ ] Homomorphic encryption (SEAL integration)
- [ ] vLLM integration for inference
- [ ] BitTorrent model distribution
- [ ] Multi-provider consensus verification
- [ ] GPU detection

## Contributing

This is a research project. Contributions welcome.

## License

MIT

## References

- [libp2p](https://libp2p.io/) — P2P networking stack
- [Microsoft SEAL](https://github.com/microsoft/SEAL) — Homomorphic encryption
- [vLLM](https://github.com/vllm-project/vllm) — Fast LLM inference
- [BitTorrent](https://en.wikipedia.org/wiki/BitTorrent) — P2P distribution protocol
