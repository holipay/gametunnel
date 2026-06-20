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

	"golang.org/x/crypto/argon2"
)

// KeySize is the length of the derived HMAC key.
const KeySize = 32

// ChallengeSize is the length of the random nonce.
const ChallengeSize = 16

// HMACSize is the length of the HMAC-SHA256 output.
const HMACSize = 32

// DeriveKey derives a 32-byte HMAC key from the room password using Argon2id.
// Room ID is used as salt to bind the key to a specific room.
// Returns nil if password is empty, or on internal error.
func DeriveKey(password, roomID string) ([]byte, error) {
	if password == "" {
		return nil, nil
	}
	// Argon2id params: 19 MiB memory, 2 iterations, 1 parallelism
	// Suitable for interactive authentication (~200ms on modern hardware)
	salt := []byte("GameTunnel:" + roomID)
	key := argon2.IDKey([]byte(password), salt, 2, 19*1024, 1, KeySize)
	if len(key) != KeySize {
		return nil, fmt.Errorf("argon2: key length mismatch: got %d, want %d", len(key), KeySize)
	}
	return key, nil
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
