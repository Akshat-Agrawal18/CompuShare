// Package he provides real Homomorphic Encryption for CompuShare using the
// CKKS (Cheon-Kim-Kim-Song) scheme via the Lattigo v6 library.
//
// CKKS enables approximate arithmetic on encrypted real/complex numbers,
// making it ideal for AI inference on encrypted data. Providers can perform
// Add, Multiply, and Rotate operations on ciphertexts without ever seeing
// the plaintext values.
//
// Architecture:
//   - Consumer generates keys locally (secret key never leaves the machine)
//   - Consumer encrypts input → ciphertext sent to providers
//   - Providers evaluate (Add/Mul/Rotate) on encrypted data
//   - Consumer decrypts the final result
//
// Serialization:
//
//	All ciphertexts are self-contained: they include the CKKS parameters
//
// alongside the encrypted data, so any provider can deserialize and operate
// on them without pre-shared key material.
package he

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"math"

	"github.com/tuneinsight/lattigo/v6/core/rlwe"
	"github.com/tuneinsight/lattigo/v6/schemes/ckks"
)

// PublicKey is the consumer's CKKS public key for encryption.
type PublicKey struct {
	pk     *rlwe.PublicKey
	params ckks.Parameters
}

// SecretKey is the consumer's CKKS secret key for decryption.
// This NEVER leaves the consumer's machine.
type SecretKey struct {
	sk     *rlwe.SecretKey
	params ckks.Parameters
}

// KeyGenerator generates CKKS key pairs for homomorphic encryption.
type KeyGenerator struct {
	params ckks.Parameters
}

// NewKeyGenerator creates a new CKKS key generator.
// keySize selects the security level:
//   - 128: ~128-bit security (logN=14, ~8192 slots)
//   - 256: ~256-bit security (logN=15, ~16384 slots)
//
// Any other value defaults to 128-bit.
func NewKeyGenerator(keySize int) (*KeyGenerator, error) {
	var paramsLiteral ckks.ParametersLiteral

	switch keySize {
	case 256:
		paramsLiteral = ckks.ParametersLiteral{
			LogN:            15,
			LogQ:            []int{55, 45, 45, 45, 45, 45, 45, 45, 45, 45},
			LogP:            []int{55, 55},
			LogDefaultScale: 45,
		}
	default: // 128 or fallback
		paramsLiteral = ckks.ExampleParameters128BitLogN14LogQP438
	}

	params, err := ckks.NewParametersFromLiteral(paramsLiteral)
	if err != nil {
		return nil, fmt.Errorf("failed to create CKKS parameters: %w", err)
	}

	return &KeyGenerator{params: params}, nil
}

// GenerateKeys generates a CKKS public/secret key pair.
// The secret key must NEVER be shared with providers.
func (kg *KeyGenerator) GenerateKeys() (*PublicKey, *SecretKey, error) {
	kgen := rlwe.NewKeyGenerator(kg.params)
	sk := kgen.GenSecretKeyNew()
	pk := kgen.GenPublicKeyNew(sk)

	return &PublicKey{pk: pk, params: kg.params},
		&SecretKey{sk: sk, params: kg.params},
		nil
}

// EncryptInput encrypts a text input string using the consumer's public key.
// The plaintext is encoded by mapping each byte to a float64 slot value [0, 255],
// padded to the CKKS slot count. Returns a self-contained serialized ciphertext.
func EncryptInput(plaintext string, pubKey *PublicKey) ([]byte, error) {
	params := pubKey.params
	slots := params.MaxSlots()

	// Convert text bytes to float64 values
	data := []byte(plaintext)
	floats := make([]float64, slots)
	for i := range floats {
		if i < len(data) {
			floats[i] = float64(data[i])
		}
	}

	// Encode into CKKS plaintext
	encoder := ckks.NewEncoder(params)
	pt := ckks.NewPlaintext(params, params.MaxLevel())
	if err := encoder.Encode(floats, pt); err != nil {
		return nil, fmt.Errorf("CKKS encode failed: %w", err)
	}

	// Encrypt using the public key
	encryptor := rlwe.NewEncryptor(params, pubKey.pk)
	ct, err := encryptor.EncryptNew(pt)
	if err != nil {
		return nil, fmt.Errorf("CKKS encrypt failed: %w", err)
	}

	return marshalCiphertext(ct, params)
}

// DecryptOutput decrypts a CKKS ciphertext using the consumer's secret key.
// This is the ONLY place where the secret key is used.
// Returns the decrypted data as a byte slice (float values rounded and clamped to [0, 255]).
func DecryptOutput(ciphertext []byte, secretKey *SecretKey) ([]byte, error) {
	params := secretKey.params

	// Deserialize the self-contained ciphertext
	container, err := unmarshalCiphertext(ciphertext)
	if err != nil {
		return nil, fmt.Errorf("failed to deserialize ciphertext: %w", err)
	}

	// Restore CKKS parameters from the container
	ctParams, err := container.getParams()
	if err != nil {
		return nil, fmt.Errorf("failed to restore CKKS parameters: %w", err)
	}

	// Use the ciphertext's own parameters for decryption since they
	// may differ from the consumer's key parameters (e.g. if the
	// ciphertext was produced by a provider with different settings)
	params = ctParams

	// Decrypt the ciphertext
	decryptor := rlwe.NewDecryptor(params, secretKey.sk)
	pt := decryptor.DecryptNew(container.Ciphertext)

	// Decode from CKKS plaintext to float64 values
	encoder := ckks.NewEncoder(params)
	var floats []complex128
	if err := encoder.Decode(pt, &floats); err != nil {
		return nil, fmt.Errorf("CKKS decode failed: %w", err)
	}

	// Convert complex128 slots back to bytes
	result := make([]byte, len(floats))
	for i, c := range floats {
		val := int(math.Round(real(c)))
		if val < 0 {
			val = 0
		} else if val > 255 {
			val = 255
		}
		result[i] = byte(val)
	}

	return result, nil
}

// EncryptVector encrypts a vector of float64 values for homomorphic computation.
// Values are padded to the CKKS slot count with zeros.
func EncryptVector(values []float64, pubKey *PublicKey) ([]byte, error) {
	params := pubKey.params
	slots := params.MaxSlots()

	// Pad to slot count
	padded := make([]float64, slots)
	copy(padded, values)

	encoder := ckks.NewEncoder(params)
	pt := ckks.NewPlaintext(params, params.MaxLevel())
	if err := encoder.Encode(padded, pt); err != nil {
		return nil, fmt.Errorf("CKKS encode failed: %w", err)
	}

	encryptor := rlwe.NewEncryptor(params, pubKey.pk)
	ct, err := encryptor.EncryptNew(pt)
	if err != nil {
		return nil, fmt.Errorf("CKKS encrypt failed: %w", err)
	}

	return marshalCiphertext(ct, params)
}

// DecryptVector decrypts a CKKS ciphertext to a vector of float64 values.
// If size > 0, only the first `size` values are returned.
func DecryptVector(ciphertext []byte, secretKey *SecretKey) ([]float64, error) {
	params := secretKey.params

	container, err := unmarshalCiphertext(ciphertext)
	if err != nil {
		return nil, fmt.Errorf("failed to deserialize ciphertext: %w", err)
	}

	ctParams, err := container.getParams()
	if err != nil {
		return nil, fmt.Errorf("failed to restore CKKS parameters: %w", err)
	}
	params = ctParams

	decryptor := rlwe.NewDecryptor(params, secretKey.sk)
	pt := decryptor.DecryptNew(container.Ciphertext)

	encoder := ckks.NewEncoder(params)
	var values []complex128
	if err := encoder.Decode(pt, &values); err != nil {
		return nil, fmt.Errorf("CKKS decode failed: %w", err)
	}

	result := make([]float64, len(values))
	for i, v := range values {
		result[i] = real(v)
	}

	return result, nil
}

// Evaluator performs homomorphic operations on encrypted data (provider-side).
// Providers can Add, Multiply, and Rotate ciphertexts without seeing plaintext.
type Evaluator struct {
	params ckks.Parameters
	eval   *ckks.Evaluator
}

// NewEvaluator creates a new CKKS evaluator for the given evaluation keys.
// Evaluation keys must be generated by the consumer's secret key.
func NewEvaluator(evalKeys *EvaluationKeys) *Evaluator {
	params := evalKeys.params

	// Build the evaluation key set
	evk := rlwe.NewMemEvaluationKeySet(evalKeys.RelinKey, evalKeys.GaloisKeys...)

	// Create the CKKS evaluator with the evaluation keys
	eval := ckks.NewEvaluator(params, evk)

	return &Evaluator{
		params: params,
		eval:   eval,
	}
}

// Add performs homomorphic addition of two CKKS ciphertexts.
// Result[i] = left[i] + right[i] for all encrypted slots.
func (e *Evaluator) Add(left, right []byte) ([]byte, error) {
	leftContainer, err := unmarshalCiphertext(left)
	if err != nil {
		return nil, fmt.Errorf("failed to deserialize left operand: %w", err)
	}
	rightContainer, err := unmarshalCiphertext(right)
	if err != nil {
		return nil, fmt.Errorf("failed to deserialize right operand: %w", err)
	}

	result, err := e.eval.AddNew(leftContainer.Ciphertext, rightContainer.Ciphertext)
	if err != nil {
		return nil, fmt.Errorf("CKKS Add failed: %w", err)
	}

	return marshalCiphertext(result, e.params)
}

// Multiply performs homomorphic multiplication of two CKKS ciphertexts.
// Result[i] = left[i] * right[i] for all encrypted slots.
// Note: without relinearization, the result has degree 2 (higher storage cost
// but correct values). The evaluator includes a relinearization key by default.
func (e *Evaluator) Multiply(left, right []byte) ([]byte, error) {
	leftContainer, err := unmarshalCiphertext(left)
	if err != nil {
		return nil, fmt.Errorf("failed to deserialize left operand: %w", err)
	}
	rightContainer, err := unmarshalCiphertext(right)
	if err != nil {
		return nil, fmt.Errorf("failed to deserialize right operand: %w", err)
	}

	result, err := e.eval.MulRelinNew(leftContainer.Ciphertext, rightContainer.Ciphertext)
	if err != nil {
		return nil, fmt.Errorf("CKKS Multiply failed: %w", err)
	}

	return marshalCiphertext(result, e.params)
}

// Rotate performs a cyclic rotation of encrypted slots by the given number of steps.
// Positive steps rotate left, negative steps rotate right.
// Requires pre-generated galois keys for the rotation amount.
func (e *Evaluator) Rotate(ct []byte, steps int) ([]byte, error) {
	container, err := unmarshalCiphertext(ct)
	if err != nil {
		return nil, fmt.Errorf("failed to deserialize ciphertext: %w", err)
	}

	result, err := e.eval.RotateNew(container.Ciphertext, steps)
	if err != nil {
		return nil, fmt.Errorf("CKKS Rotate failed: %w", err)
	}

	return marshalCiphertext(result, e.params)
}

// ---------------------------------------------------------------------------
// Internal serialization helpers
// ---------------------------------------------------------------------------

// ciphertextContainer is a self-contained serializable wrapper that bundles
// the CKKS parameters with the ciphertext. This allows any recipient to
// deserialize and operate on the ciphertext without pre-shared parameters.
type ciphertextContainer struct {
	// ParamsData holds the marshaled CKKS parameters (logN, moduli, scale, etc.)
	ParamsData []byte
	// Ciphertext is the encrypted CKKS ciphertext
	Ciphertext *rlwe.Ciphertext
}

// ckksParams is a gob-serializable representation of CKKS parameters.
// We use a separate type because ckks.Parameters contains private fields
// that gob cannot serialize directly.
type ckksParams struct {
	ParamsData []byte
}

// marshalCiphertext serializes a ciphertext together with its CKKS parameters
// into a self-contained byte slice.
func marshalCiphertext(ct *rlwe.Ciphertext, params ckks.Parameters) ([]byte, error) {
	// Marshal the CKKS parameters
	paramBytes, err := params.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("failed to marshal params: %w", err)
	}

	// Marshal the ciphertext using Lattigo's binary format
	ctBytes, err := ct.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("failed to marshal ciphertext: %w", err)
	}

	// Use a buffer to write: params length (4 bytes) + params + ciphertext
	var buf bytes.Buffer

	// Write parameter data with its length prefix
	paramLen := uint32(len(paramBytes))
	buf.WriteByte(byte(paramLen >> 24))
	buf.WriteByte(byte(paramLen >> 16))
	buf.WriteByte(byte(paramLen >> 8))
	buf.WriteByte(byte(paramLen))
	buf.Write(paramBytes)

	// Write ciphertext data
	buf.Write(ctBytes)

	return buf.Bytes(), nil
}

// unmarshalCiphertext deserializes a self-contained byte slice back into
// a ciphertextContainer with the CKKS parameters and ciphertext restored.
func unmarshalCiphertext(data []byte) (*ciphertextContainer, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("ciphertext data too short: %d bytes", len(data))
	}

	// Read parameter data length
	paramLen := uint32(data[0])<<24 | uint32(data[1])<<16 | uint32(data[2])<<8 | uint32(data[3])
	if uint64(len(data)) < uint64(4+paramLen) {
		return nil, fmt.Errorf("ciphertext data truncated: expected at least %d bytes, got %d", 4+paramLen, len(data))
	}

	paramBytes := data[4 : 4+paramLen]
	ctBytes := data[4+paramLen:]

	// Restore CKKS parameters from their binary representation
	var params ckks.Parameters
	if err := params.UnmarshalBinary(paramBytes); err != nil {
		return nil, fmt.Errorf("failed to unmarshal CKKS parameters: %w", err)
	}

	// Restore the ciphertext using Lattigo's binary format
	ct := &rlwe.Ciphertext{}
	if err := ct.UnmarshalBinary(ctBytes); err != nil {
		return nil, fmt.Errorf("failed to unmarshal ciphertext: %w", err)
	}

	return &ciphertextContainer{
		ParamsData: paramBytes,
		Ciphertext: ct,
	}, nil
}

// getParams restores the CKKS parameters from the container's binary data.
func (c *ciphertextContainer) getParams() (ckks.Parameters, error) {
	var params ckks.Parameters
	if err := params.UnmarshalBinary(c.ParamsData); err != nil {
		return ckks.Parameters{}, fmt.Errorf("failed to restore CKKS parameters: %w", err)
	}
	return params, nil
}

// init registers types with gob for potential future use.
func init() {
	gob.Register(&rlwe.Ciphertext{})
}
