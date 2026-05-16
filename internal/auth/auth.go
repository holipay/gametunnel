// Package auth implements HMAC challenge-response authentication for GameTunnel.
//
// Key derivation: HKDF-SHA256(password, info="GameTunnel:"+roomID) → 32-byte key
// Challenge-response: HMAC-SHA256(key, challenge || roomID || username || clientAddr)
//
// The password never leaves the client. Even if an attacker captures the challenge
// and response, they cannot recover the password within a reasonable time.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"net"

	"golang.org/x/crypto/hkdf"
)

// KeySize is the length of the derived HMAC key.
const KeySize = 32

// ChallengeSize is the length of the random nonce.
const ChallengeSize = 16

// HMACSize is the length of the HMAC-SHA256 output.
const HMACSize = 32

// ChallengeTimeout is the maximum time allowed for a client to respond to a challenge.
// AuthExpiry is used by the server to expire stale pending auth entries.
// These are durations, defined here for reference; the server uses them directly.

// DeriveKey derives a 32-byte HMAC key from the room password using HKDF-SHA256.
// Room ID is used as "info" context to bind the key to a specific room.
// Returns nil if password is empty.
func DeriveKey(password, roomID string) []byte {
	if password == "" {
		return nil
	}
	hkdfReader := hkdf.New(sha256.New, []byte(password), nil, []byte("GameTunnel:"+roomID))
	key := make([]byte, KeySize)
	if _, err := io.ReadFull(hkdfReader, key); err != nil {
		return nil
	}
	return key
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
func ComputeHMAC(key []byte, challenge []byte, roomID, username string, remoteAddr *net.UDPAddr) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(challenge)
	mac.Write([]byte(roomID))
	mac.Write([]byte(username))
	if remoteAddr != nil {
		mac.Write([]byte(remoteAddr.String()))
	}
	return mac.Sum(nil)
}

// VerifyHMAC verifies the client's auth HMAC. Returns true if valid.
func VerifyHMAC(key, clientHMAC []byte, challenge []byte, roomID, username string, remoteAddr *net.UDPAddr) bool {
	expected := ComputeHMAC(key, challenge, roomID, username, remoteAddr)
	return hmac.Equal(clientHMAC, expected)
}
