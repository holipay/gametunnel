package auth

import (
	"bytes"
	"net"
	"testing"
)

func TestDeriveKeyEmptyPassword(t *testing.T) {
	key := DeriveKey("", "room1")
	if key != nil {
		t.Fatalf("expected nil for empty password, got %v", key)
	}
}

func TestDeriveKeyDeterministic(t *testing.T) {
	key1 := DeriveKey("secret", "room1")
	key2 := DeriveKey("secret", "room1")
	if !bytes.Equal(key1, key2) {
		t.Fatal("same inputs should produce same key")
	}
}

func TestDeriveKeyDifferentRoom(t *testing.T) {
	key1 := DeriveKey("secret", "room1")
	key2 := DeriveKey("secret", "room2")
	if bytes.Equal(key1, key2) {
		t.Fatal("different rooms should produce different keys")
	}
}

func TestDeriveKeyDifferentPassword(t *testing.T) {
	key1 := DeriveKey("pass1", "room1")
	key2 := DeriveKey("pass2", "room1")
	if bytes.Equal(key1, key2) {
		t.Fatal("different passwords should produce different keys")
	}
}

func TestDeriveKeyLength(t *testing.T) {
	key := DeriveKey("secret", "room1")
	if len(key) != KeySize {
		t.Fatalf("key length: got %d, want %d", len(key), KeySize)
	}
}

func TestGenerateChallengeLength(t *testing.T) {
	c, err := GenerateChallenge()
	if err != nil {
		t.Fatalf("GenerateChallenge failed: %v", err)
	}
	if len(c) != ChallengeSize {
		t.Fatalf("challenge length: got %d, want %d", len(c), ChallengeSize)
	}
}

func TestGenerateChallengeUnique(t *testing.T) {
	c1, _ := GenerateChallenge()
	c2, _ := GenerateChallenge()
	if bytes.Equal(c1, c2) {
		t.Fatal("two challenges should not be equal")
	}
}

func TestComputeAndVerifyHMAC(t *testing.T) {
	key := DeriveKey("testpassword", "room1")
	challenge, _ := GenerateChallenge()
	addr := &net.UDPAddr{IP: net.IPv4(192, 168, 1, 100), Port: 12345}

	hmacVal := ComputeHMAC(key, challenge, "room1", "Player1", addr)
	if len(hmacVal) != HMACSize {
		t.Fatalf("HMAC length: got %d, want %d", len(hmacVal), HMACSize)
	}

	if !VerifyHMAC(key, hmacVal, challenge, "room1", "Player1", addr) {
		t.Fatal("VerifyHMAC should pass for correct HMAC")
	}
}

func TestVerifyHMACWrongKey(t *testing.T) {
	key := DeriveKey("correct", "room1")
	wrongKey := DeriveKey("wrong", "room1")
	challenge, _ := GenerateChallenge()
	addr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 1000}

	hmacVal := ComputeHMAC(key, challenge, "room1", "user", addr)
	if VerifyHMAC(wrongKey, hmacVal, challenge, "room1", "user", addr) {
		t.Fatal("should not verify with wrong key")
	}
}

func TestVerifyHMACWrongChallenge(t *testing.T) {
	key := DeriveKey("secret", "room1")
	challenge, _ := GenerateChallenge()
	wrongChallenge, _ := GenerateChallenge()
	addr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 1000}

	hmacVal := ComputeHMAC(key, challenge, "room1", "user", addr)
	if VerifyHMAC(key, hmacVal, wrongChallenge, "room1", "user", addr) {
		t.Fatal("should not verify with wrong challenge")
	}
}

func TestVerifyHMACWrongRoom(t *testing.T) {
	key := DeriveKey("secret", "room1")
	challenge, _ := GenerateChallenge()
	addr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 1000}

	hmacVal := ComputeHMAC(key, challenge, "room1", "user", addr)
	if VerifyHMAC(key, hmacVal, challenge, "room2", "user", addr) {
		t.Fatal("should not verify with wrong roomID")
	}
}

func TestVerifyHMACWrongUsername(t *testing.T) {
	key := DeriveKey("secret", "room1")
	challenge, _ := GenerateChallenge()
	addr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 1000}

	hmacVal := ComputeHMAC(key, challenge, "room1", "Alice", addr)
	if VerifyHMAC(key, hmacVal, challenge, "room1", "Bob", addr) {
		t.Fatal("should not verify with wrong username")
	}
}

func TestVerifyHMACWrongAddr(t *testing.T) {
	key := DeriveKey("secret", "room1")
	challenge, _ := GenerateChallenge()
	addr1 := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 1000}
	addr2 := &net.UDPAddr{IP: net.IPv4(5, 6, 7, 8), Port: 2000}

	hmacVal := ComputeHMAC(key, challenge, "room1", "user", addr1)
	if VerifyHMAC(key, hmacVal, challenge, "room1", "user", addr2) {
		t.Fatal("should not verify with wrong address")
	}
}

func TestVerifyHMACNilAddr(t *testing.T) {
	key := DeriveKey("secret", "room1")
	challenge, _ := GenerateChallenge()

	hmacVal := ComputeHMAC(key, challenge, "room1", "user", nil)
	if !VerifyHMAC(key, hmacVal, challenge, "room1", "user", nil) {
		t.Fatal("should verify with nil address")
	}
}

func TestFullAuthFlow(t *testing.T) {
	password := "myroomsecret"
	roomID := "gaming-room"
	username := "ProPlayer"
	serverAddr := &net.UDPAddr{IP: net.IPv4(203, 0, 113, 1), Port: 4700}

	serverKey := DeriveKey(password, roomID)
	challenge, _ := GenerateChallenge()

	clientKey := DeriveKey(password, roomID)
	if !bytes.Equal(serverKey, clientKey) {
		t.Fatal("client and server keys should match")
	}

	clientHMAC := ComputeHMAC(clientKey, challenge, roomID, username, serverAddr)
	if !VerifyHMAC(serverKey, clientHMAC, challenge, roomID, username, serverAddr) {
		t.Fatal("full auth flow: verification should pass")
	}
}
