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
	"sync"
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

	// replayWindowSize is the number of recent counter values tracked for replay detection.
	// A window of 64 allows packets to arrive up to 64 positions out of order,
	// which is generous for UDP gaming traffic where reordering is typically <10 packets.
	replayWindowSize = 64
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

	// Replay protection: sliding window of recently received counter values.
	// The window tracks counters in range [highestCounter - windowSize + 1, highestCounter].
	// Packets with counters outside this window (too old or too far ahead) are rejected.
	// This prevents replay attacks without requiring strict in-order delivery,
	// which is important for UDP where packets may arrive slightly out of order.
	replayMu       sync.Mutex
	highestCounter uint64 // highest counter value seen, protected by replayMu
	replayBitmap   uint64 // bitmap of received counters relative to highestCounter
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
	if err := c.initCounter(); err != nil {
		return nil, err
	}
	return c, nil
}

// initCounter sets the counter to a random 48-bit value.
// This prevents nonce reuse when a client reconnects and creates new ciphers
// with the same key (derived from the room password). Without randomization,
// both sides would start at counter=0, producing identical nonces — fatal
// for ChaCha20-Poly1305. A 48-bit random space gives >281 trillion possible
// starting values, making collision probability negligible.
func (c *Cipher) initCounter() error {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Errorf("crypto: cannot generate random nonce: %w", err)
	}
	// Use lower 48 bits of random 8-byte value
	c.counter.Store(binary.LittleEndian.Uint64(b[:]) & ((1 << 48) - 1))
	return nil
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
	out := make([]byte, 0, Overhead+len(plaintext))
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
// Includes replay protection via a sliding window that rejects duplicate
// or too-old counter values in the nonce.
func (c *Cipher) Decrypt(data []byte) ([]byte, error) {
	return c.DecryptInto(nil, data)
}

// DecryptInto decrypts data into a caller-provided buffer, appending
// the plaintext after existing content. Pass nil for dst to allocate
// a new buffer (same as Decrypt). Returns the extended slice.
func (c *Cipher) DecryptInto(dst, data []byte) ([]byte, error) {
	if len(data) < Overhead {
		return nil, errors.New("crypto: encrypted data too short")
	}
	if data[0] != EncVersion {
		return nil, fmt.Errorf("crypto: unsupported encryption version %d", data[0])
	}
	nonce := data[1 : 1+NonceSize]
	ciphertext := data[1+NonceSize:]

	// Extract counter from nonce (first 8 bytes, little-endian)
	ctr := binary.LittleEndian.Uint64(nonce[0:8])

	// Replay protection: check if this counter is within the valid window
	if !c.checkReplayWindow(ctr) {
		return nil, errors.New("crypto: replay detected (counter out of window)")
	}

	plaintext, err := c.aead.Open(dst, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("crypto: decrypt: %w", err)
	}
	return plaintext, nil
}

// checkReplayWindow verifies that the counter is not a replay and updates the window.
// Returns true if the counter is valid (not a replay), false if it's a duplicate or too old.
// Uses a bitmap-based sliding window: tracks counters in range [highest-counterWindowSize+1, highest].
func (c *Cipher) checkReplayWindow(ctr uint64) bool {
	c.replayMu.Lock()
	defer c.replayMu.Unlock()

	highest := c.highestCounter

	if ctr > highest {
		shift := ctr - highest
		if shift < replayWindowSize {
			c.replayBitmap = (c.replayBitmap << shift) | 1
		} else {
			c.replayBitmap = 1
		}
		c.highestCounter = ctr
		return true
	}

	diff := highest - ctr
	if diff >= replayWindowSize {
		return false
	}
	bit := uint64(1) << diff
	if c.replayBitmap&bit != 0 {
		return false
	}
	c.replayBitmap |= bit
	return true
}

// IsEncrypted checks if data starts with the encryption version byte.
// Used to distinguish encrypted vs plaintext packets during transition.
func IsEncrypted(data []byte) bool {
	return len(data) > 0 && data[0] == EncVersion
}
