// Package auth implements HMAC challenge-response authentication for GameTunnel.
//
// Key derivation: argon2.IDKey(password, salt=roomID, 32 bytes) → 32-byte key
// Challenge-response: HMAC-SHA256(key, challenge || len(roomID) || roomID || len(username) || username || len(addr) || addr)
//
// The password never leaves the client. Even if an attacker captures the challenge
// and response, they cannot recover the password within a reasonable time.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

// KeySize is the length of the derived HMAC key.
const KeySize = 32

// ChallengeSize is the length of the random nonce.
const ChallengeSize = 16

// HMACSize is the length of the HMAC-SHA256 output.
const HMACSize = 32

// keyCacheTTL is how long derived keys are kept in cache.
// Entries older than this are evicted on access or during periodic cleanup.
const keyCacheTTL = 10 * time.Minute

// cachedKey holds a derived key with its creation time for TTL-based eviction.
type cachedKey struct {
	key       []byte
	createdAt time.Time
}

// keyCache caches derived Argon2 keys to avoid repeated ~200ms computation.
// Entries are evicted after keyCacheTTL to avoid retaining stale derived keys
// indefinitely. Lazy eviction happens on Load; periodic cleanup via
// CleanupKeyCache handles entries that are never accessed again.
var keyCache struct {
	sync.Mutex
	entries map[string]cachedKey
}

func init() {
	keyCache.entries = make(map[string]cachedKey)
}

// DeriveKey derives a 32-byte HMAC key from the room password using Argon2id.
// Room ID is used as salt to bind the key to a specific room.
// Returns nil if password is empty, or on internal error.
// Results are cached with TTL — repeated calls within the TTL window are instant.
func DeriveKey(password, roomID string) ([]byte, error) {
	if password == "" {
		return nil, nil
	}

	cacheKey := password + ":" + roomID
	now := time.Now()

	keyCache.Lock()
	if entry, ok := keyCache.entries[cacheKey]; ok {
		if now.Sub(entry.createdAt) < keyCacheTTL {
			keyCache.Unlock()
			return entry.key, nil
		}
		delete(keyCache.entries, cacheKey)
	}
	keyCache.Unlock()

	// Argon2id params: 19 MiB memory, 2 iterations, 1 parallelism
	// On modern hardware ~200ms; may be slower on low-end ARM devices (OpenWrt routers).
	salt := []byte("GameTunnel:" + roomID)
	key := argon2.IDKey([]byte(password), salt, 2, 19*1024, 1, KeySize)
	if len(key) != KeySize {
		return nil, fmt.Errorf("argon2: key length mismatch: got %d, want %d", len(key), KeySize)
	}

	// Cache a copy so the caller can't mutate the cached value
	cached := make([]byte, KeySize)
	copy(cached, key)

	keyCache.Lock()
	keyCache.entries[cacheKey] = cachedKey{key: cached, createdAt: now}
	keyCache.Unlock()

	return cached, nil
}

// CleanupKeyCache removes expired entries from the key cache.
// Should be called periodically (e.g. every 5 minutes) to prevent
// unbounded memory growth from entries that are never accessed again.
func CleanupKeyCache() {
	now := time.Now()
	keyCache.Lock()
	for k, entry := range keyCache.entries {
		if now.Sub(entry.createdAt) >= keyCacheTTL {
			delete(keyCache.entries, k)
		}
	}
	keyCache.Unlock()
}

// GenerateChallenge creates a random nonce for authentication.
func GenerateChallenge() ([]byte, error) {
	nonce := make([]byte, ChallengeSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate challenge: %w", err)
	}
	return nonce, nil
}

// ComputeHMAC computes the HMAC-SHA256 over the challenge and context.
// Binds the response to: challenge nonce, room ID, username, and client address.
// Each variable-length field is length-prefixed to prevent field-boundary ambiguity.
func ComputeHMAC(key []byte, challenge []byte, roomID, username string, remoteAddr *net.UDPAddr) []byte {
	if len(key) == 0 {
		return nil
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(challenge)
	// Length-prefix each variable-length field
	var lenBuf [2]byte
	binary.BigEndian.PutUint16(lenBuf[:], uint16(len(roomID)))
	mac.Write(lenBuf[:])
	mac.Write([]byte(roomID))
	binary.BigEndian.PutUint16(lenBuf[:], uint16(len(username)))
	mac.Write(lenBuf[:])
	mac.Write([]byte(username))
	if remoteAddr != nil {
		addrStr := remoteAddr.String()
		binary.BigEndian.PutUint16(lenBuf[:], uint16(len(addrStr)))
		mac.Write(lenBuf[:])
		mac.Write([]byte(addrStr))
	} else {
		binary.BigEndian.PutUint16(lenBuf[:], 0)
		mac.Write(lenBuf[:])
	}
	return mac.Sum(nil)
}

// VerifyHMAC verifies the client's auth HMAC. Returns true if valid.
func VerifyHMAC(key, clientHMAC []byte, challenge []byte, roomID, username string, remoteAddr *net.UDPAddr) bool {
	if len(key) == 0 || len(clientHMAC) == 0 {
		return false
	}
	expected := ComputeHMAC(key, challenge, roomID, username, remoteAddr)
	return hmac.Equal(clientHMAC, expected)
}
