# CompuShare Protocol Specification

## Overview

CompuShare is a P2P distributed compute network for AI inference. This document specifies the protocols used for node communication, capability discovery, inference execution, and result verification.

## Table of Contents

1. [Network Protocol](#network-protocol)
2. [Capability Advertisement](#capability-advertisement)
3. [Inference Pipeline](#inference-pipeline)
4. [Model Distribution](#model-distribution)
5. [Karma System](#karma-system)
6. [Verification](#verification)

---

## Network Protocol

### Transport

All communication uses **libp2p** with the following transports:

- TCP for reliable streaming
- QUIC for low-latency streaming
- WebSocket for browser compatibility
- Circuit Relay for NAT traversal

### Protocols

Each protocol uses a versioned path prefix: `/compushare/<service>/<version>`

| Protocol | Path | Direction | Purpose |
|----------|------|----------|---------|
| Inference | `/compushare/inference/1.0` | Bidirectional | Run inference on encrypted data |
| Model Fetch | `/compushare/model/1.0` | Provider → Consumer | Download model chunks |
| Karma | `/compushare/karma/1.0` | Bidirectional | Propagate karma scores |
| Health | `/compushare/health/1.0` | Bidirectional | Liveness probes |

### Message Envelope

All messages use Protocol Buffers (protobuf):

```protobuf
message Envelope {
  string request_id = 1;
  string protocol = 2;
  bytes payload = 3;
  bytes signature = 4;
  int64 timestamp = 5;
}
```

---

## Capability Advertisement

### Overview

Nodes advertise their hardware capabilities via **GossipSub** pub/sub on the topic `compushare:providers`.

### Capability Manifest

```json
{
  "peer_id": "QmXYZ...",
  "timestamp": 1711500000,
  "gpu": {
    "model": "NVIDIA RTX 4090",
    "vram_gb": 24,
    "compute_units": 16384,
    "cuda_version": "12.1"
  },
  "cpu": {
    "model": "AMD Ryzen 9 7950X",
    "cores": 16,
    "threads": 32
  },
  "ram_gb": 32,
  "bandwidth_mbps": 1000,
  "region": "us-west-2",
  "is_provider": true,
  "models_available": ["tinyllama-1b", "llama-7b-q4"],
  "signature": "base64..."
}
```

### Discovery Flow

```
1. Provider boots up
2. Provider creates signed CapabilityManifest
3. Provider publishes to GossipSub topic every 60 seconds
4. Consumer subscribes to topic
5. Consumer receives all provider manifests
6. Consumer filters by requirements (VRAM, RAM, region, etc.)
7. Consumer selects top-N providers by karma score
```

### Kademlia DHT Integration

Providers register themselves in the DHT under capability keys:

- `providers:gpu:24gb:us-west` → peer addresses
- `providers:gpu:8gb:eu-central` → peer addresses
- `providers:ram:16gb` → peer addresses

This enables direct peer connections without pub/sub.

---

## Inference Pipeline

### Overview

The inference pipeline uses **pipeline parallelism** to distribute a model across multiple providers. Each provider runs a subset of layers.

### Pipeline Stages

```
Consumer                    Provider A           Provider B           Provider C
   |                            |                    |                    |
   |  Encrypt(input)            |                    |                    |
   |--------------------------->|                    |                    |
   |                            |                    |                    |
   |  [Layers 0-15]             |                    |                    |
   |===========================>|                    |                    |
   |                            |                    |                    |
   |  Encrypted activations     |                    |                    |
   |<==========================|                    |                    |
   |                            |                    |                    |
   |                            |  [Layers 16-31]    |                    |
   |                            |===================>|                    |
   |                            |                    |                    |
   |                            |  Encrypted activations                    |
   |                            |<==================|                    |
   |                            |                    |                    |
   |                            |                    |  [Layers 32-47]    |
   |                            |                    |===================>|
   |                            |                    |                    |
   |                            |                    |  Encrypted output   |
   |                            |                    |<===================|
   |                            |                    |                    |
   |  Encrypted final output    |                    |                    |
   |<==============================================|                    |
   |                            |                    |                    |
   |  Decrypt(output)           |                    |                    |
   v                            v                    v                    v
```

### Inference Request

```protobuf
message InferenceRequest {
  string request_id = 1;
  string model_id = 2;
  bytes encrypted_input = 3;
  int32 max_tokens = 4;
  float temperature = 5;
  string consumer_peer_id = 6;
  repeated PipelineStage stages = 7;
  bytes request_signature = 8;
}

message PipelineStage {
  int32 stage_id = 1;
  string provider_peer_id = 2;
  int32 start_layer = 3;
  int32 end_layer = 4;
  bytes encrypted_state = 5;
}
```

### Inference Response

```protobuf
message InferenceResponse {
  string request_id = 1;
  int32 stage_id = 2;
  bytes encrypted_activations = 3;
  int64 execution_time_ms = 4;
  bytes checkpoint_hash = 5;
  bytes stage_signature = 6;
}
```

---

## Model Distribution

### Overview

Models are distributed using **BitTorrent-style chunking**. Model chunks are content-addressed and seeded across providers.

### Model Manifest

```json
{
  "model_id": "llama-7b-q4",
  "version": "v1.0",
  "total_size_gb": 3.5,
  "piece_size_mb": 1,
  "pieces": [
    {
      "piece_id": 0,
      "content_hash": "QmWAT...YY",
      "layers": [0, 1, 2],
      "size_mb": 1
    },
    ...
  ],
  "signature": "base64..."
}
```

### Torrent Protocol

1. **Trackerless**: Uses Kademlia DHT for peer discovery
2. **Piece selection**: Rarest-first
3. **Verification**: SHA-256 hash verification per piece
4. **Seeding**: All available pieces announced via DHT

### Model Loading

Providers pre-cache models they frequently serve:

1. Model manifest fetched from DHT
2. Missing pieces requested from peers via rarest-first
3. Pieces verified and assembled
4. Model loaded into vLLM

---

## Karma System

### Overview

Karma is a reputation score that tracks how much compute a node has contributed. It does NOT affect priority — all nodes get equal access.

### Karma Calculation

```
karma = 1.0 + log10(total_contribution_ms / 1_000_000)
```

- New nodes start at karma = 1.0
- Each successful layer execution adds to karma
- Failures reduce karma by 5%
- Karma is stored in the DHT and propagates via GossipSub

### Karma Message

```protobuf
message KarmaUpdate {
  string peer_id = 1;
  int64 total_contribution_ms = 2;
  int32 total_requests = 3;
  int32 successful_requests = 4;
  float karma_score = 5;
  int64 timestamp = 6;
  bytes signature = 7;
}
```

---

## Verification

### Overview

Verification ensures result integrity without TEE. Uses **multi-provider consensus** and **statistical sampling**.

### Consensus Protocol

```
1. Consumer requests inference from N providers
2. All N providers execute inference independently
3. Results returned with result hashes (SHA-256 of tokens)
4. If >= 2/3 results match: consensus achieved
5. Disagreeing providers: karma penalty
6. Statistical sampler: 5% of results re-verified
```

### Verification Types

| Type | Trigger | Action |
|------|---------|--------|
| **Hash Match** | All providers return same hash | Accept result |
| **Majority Vote** | >= 2/3 match | Accept majority, penalize dissenters |
| **Statistical Sample** | 5% random | Re-run with different providers |
| **Checkpoint Challenge** | Provider accused | Verify Merkle checkpoint |

### Merkle Checkpointing

Providers commit to model state before execution:

```protobuf
message Checkpoint {
  string request_id = 1;
  string model_id = 2;
  int32 start_layer = 3;
  int32 end_layer = 4;
  string merkle_root = 5;
  int64 timestamp = 6;
  bytes signature = 7;
}
```

---

## Security Considerations

### Homomorphic Encryption

- Scheme: **CKKS** (approximate arithmetic)
- Parameters: poly_modulus_degree=8192, coeff_modulus=60+60+60 bits
- Security: ~128-bit

### Connection Security

- All connections: **mTLS** with libp2p
- Private networks: **pnet** (pre-shared key)
- NAT traversal: **circuit relay**

### Attack Mitigation

| Attack | Mitigation |
|--------|------------|
| Sybil | Karma requires sustained contribution |
| Eclipse | DHT authenticated peer lists |
| DoS | Rate limiting, peer scoring |
| Result poisoning | Multi-provider consensus |
| Data theft | HE encryption, container isolation |

---

## Version History

| Version | Date | Changes |
|---------|------|---------|
| 1.0.0 | 2024-XX-XX | Initial protocol specification |
