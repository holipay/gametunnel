// Package crypto implements end-to-end encryption for GameTunnel data packets.
//
// Algorithm: ChaCha20-Poly1305 (AEAD)
// Key: derived from room password via HKDF-SHA256 (same key as auth)
// Nonce: 12 bytes = 8-byte counter + 4-byte direction tag
//
// Wire format of encrypted payload:
//
//	[1 byte: encVersion] [12 bytes: nonce] [N bytes: ciphertext] [16 bytes: Poly1305 tag]
//
// The server relays encrypted bytes transparently — no decryption needed server-side.
// P2P direct traffic uses the same encryption (both clients share the password-derived key).
package crypto

import (
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"

	"golang.org/x/crypto/chacha20poly1305"
)

const (
	// EncVersion is the current encryption format version.
	EncVersion byte = 1

	// KeySize is the ChaCha20-Poly1305 key size (256-bit).
	KeySize = 32

	// NonceSize is the ChaCha20-Poly1305 nonce size (96-bit).
	NonceSize = 12

	// TagSize is the Poly1305 authentication tag size (128-bit).
	TagSize = 16

	// Overhead is the total encryption overhead per packet.
	Overhead = 1 + NonceSize + TagSize // 1 + 12 + 16 = 29 bytes
)

// Direction tags to prevent nonce reuse across send/receive.
var (
	DirClientToServer = []byte{0x00, 0x00, 0x00, 0x01}
	DirServerToClient = []byte{0x00, 0x00, 0x00, 0x02}
	DirClientToClient = []byte{0x00, 0x00, 0x00, 0x03}
)

// Cipher performs ChaCha20-Poly1305 encryption/decryption.
type Cipher struct {
	aead     cipher.AEAD
	mu       sync.Mutex
	counter  uint64
	dirTag   []byte
}

// NewCipher creates a new Cipher with the given key and direction.
// Key must be 32 bytes (from HKDF derivation). dirTag is 4 bytes.
func NewCipher(key []byte, dirTag []byte) (*Cipher, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("crypto: key must be %d bytes, got %d", KeySize, len(key))
	}
	if len(dirTag) != 4 {
		return nil, fmt.Errorf("crypto: dirTag must be 4 bytes, got %d", len(dirTag))
	}
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: chacha20poly1305: %w", err)
	}
	return &Cipher{
		aead:   aead,
		dirTag: dirTag,
	}, nil
}

// makeNonce builds a 12-byte nonce: 8-byte counter + 4-byte direction tag.
func (c *Cipher) makeNonce() []byte {
	c.mu.Lock()
	c.counter++
	ctr := c.counter
	c.mu.Unlock()

	nonce := make([]byte, NonceSize)
	binary.LittleEndian.PutUint64(nonce[0:8], ctr)
	copy(nonce[8:12], c.dirTag)
	return nonce
}

// Encrypt encrypts plaintext and returns: [encVersion(1)] [nonce(12)] [ciphertext+tag(N+16)].
// Returns nil if plaintext is nil.
func (c *Cipher) Encrypt(plaintext []byte) []byte {
	if plaintext == nil {
		return nil
	}
	nonce := c.makeNonce()
	ciphertext := c.aead.Seal(nil, nonce, plaintext, nil)

	// Build output: version + nonce + ciphertext (includes tag)
	out := make([]byte, 0, Overhead+len(ciphertext))
	out = append(out, EncVersion)
	out = append(out, nonce...)
	out = append(out, ciphertext...)
	return out
}

// Decrypt decrypts data produced by Encrypt.
// Input format: [encVersion(1)] [nonce(12)] [ciphertext+tag(N+16)].
// Returns the original plaintext or an error.
func (c *Cipher) Decrypt(data []byte) ([]byte, error) {
	if len(data) < Overhead {
		return nil, errors.New("crypto: encrypted data too short")
	}
	if data[0] != EncVersion {
		return nil, fmt.Errorf("crypto: unsupported encryption version %d", data[0])
	}
	nonce := data[1 : 1+NonceSize]
	ciphertext := data[1+NonceSize:]

	plaintext, err := c.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("crypto: decrypt: %w", err)
	}
	return plaintext, nil
}

// IsEncrypted checks if data starts with the encryption version byte.
// Used to distinguish encrypted vs plaintext packets during transition.
func IsEncrypted(data []byte) bool {
	return len(data) > 0 && data[0] == EncVersion
}
