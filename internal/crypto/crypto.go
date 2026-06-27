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
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"sync/atomic"

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
	counter  atomic.Uint64 // 64-bit counter; nonce uses all 8 bytes. Collision risk: 2^64 packets ≈ impossible.
	dirTag   []byte
}

// NewCipher creates a new Cipher with the given key and direction.
// Key must be 32 bytes (from HKDF derivation). dirTag is 4 bytes.
// Counter is initialized to a random value to prevent nonce reuse across reconnects.
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
	c := &Cipher{
		aead:   aead,
		dirTag: append([]byte(nil), dirTag...),
	}
	c.initCounter()
	return c, nil
}

// initCounter sets the counter to a random 48-bit value.
// This prevents nonce reuse when a client reconnects and creates new ciphers
// with the same key (derived from the room password). Without randomization,
// both sides would start at counter=0, producing identical nonces — fatal
// for ChaCha20-Poly1305. A 48-bit random space gives >281 trillion possible
// starting values, making collision probability negligible.
func (c *Cipher) initCounter() {
	max := new(big.Int).SetInt64(1 << 48)
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		// Fallback: use lower 48 bits of a random 8-byte read
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			panic(fmt.Sprintf("crypto: cannot generate random nonce: %v", err))
		}
		c.counter.Store(binary.LittleEndian.Uint64(b[:]) & ((1 << 48) - 1))
		return
	}
	c.counter.Store(n.Uint64())
}

// makeNonce builds a 12-byte nonce: 8-byte counter + 4-byte direction tag.
// Uses a per-goroutine scratch buffer to avoid allocation on every encrypt call.
func (c *Cipher) makeNonce(buf *[NonceSize]byte) {
	ctr := c.counter.Add(1)

	binary.LittleEndian.PutUint64(buf[0:8], ctr)
	copy(buf[8:12], c.dirTag)
}

// Encrypt encrypts plaintext and returns: [encVersion(1)] [nonce(12)] [ciphertext+tag(N+16)].
// Returns nil if plaintext is nil.
func (c *Cipher) Encrypt(plaintext []byte) []byte {
	if plaintext == nil {
		return nil
	}
	var nonceBuf [NonceSize]byte
	c.makeNonce(&nonceBuf)

	// Seal directly into output buffer to avoid intermediate allocation.
	out := make([]byte, 0, Overhead+len(plaintext)+TagSize)
	out = append(out, EncVersion)
	out = append(out, nonceBuf[:]...)
	out = c.aead.Seal(out, nonceBuf[:], plaintext, nil)
	return out
}

// EncryptTo encrypts plaintext into dst, appending after existing content.
// Returns the extended slice. Caller must ensure dst has enough capacity:
// cap(dst) - len(dst) >= Overhead + len(plaintext) + TagSize.
// This avoids allocation when the caller provides a pre-sized buffer.
func (c *Cipher) EncryptTo(dst []byte, plaintext []byte) []byte {
	if plaintext == nil {
		return dst
	}
	var nonceBuf [NonceSize]byte
	c.makeNonce(&nonceBuf)

	dst = append(dst, EncVersion)
	dst = append(dst, nonceBuf[:]...)
	dst = c.aead.Seal(dst, nonceBuf[:], plaintext, nil)
	return dst
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
