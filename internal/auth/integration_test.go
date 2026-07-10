package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"testing"

	"golang.org/x/crypto/hkdf"
)

func TestFullAuthFlow_HKDFKeyDerivation(t *testing.T) {
	password := "2eFBhdrTxAdt"
	roomID := "default"
	username := "E14"
	addr, _ := net.ResolveUDPAddr("udp", "180.106.219.224:18127")

	// Server-side: derive key
	serverKey := DeriveKey(password, roomID)
	fmt.Printf("Server key: %s\n", hex.EncodeToString(serverKey))

	// Client-side: derive key (same HKDF)
	hkdfReader := hkdf.New(sha256.New, []byte(password), nil, []byte("GameTunnel:"+roomID))
	clientKey := make([]byte, 32)
	io.ReadFull(hkdfReader, clientKey)
	fmt.Printf("Client key: %s\n", hex.EncodeToString(clientKey))

	if hex.EncodeToString(serverKey) != hex.EncodeToString(clientKey) {
		t.Fatal("Keys don't match")
	}

	// Generate challenge
	challenge := make([]byte, 16)
	rand.Read(challenge)
	fmt.Printf("Challenge: %s\n", hex.EncodeToString(challenge))

	// Client computes HMAC
	mac := hmac.New(sha256.New, clientKey)
	mac.Write(challenge)
	var lenBuf [2]byte
	binary.BigEndian.PutUint16(lenBuf[:], uint16(len(roomID)))
	mac.Write(lenBuf[:])
	mac.Write([]byte(roomID))
	binary.BigEndian.PutUint16(lenBuf[:], uint16(len(username)))
	mac.Write(lenBuf[:])
	mac.Write([]byte(username))
	addrStr := addr.String()
	binary.BigEndian.PutUint16(lenBuf[:], uint16(len(addrStr)))
	mac.Write(lenBuf[:])
	mac.Write([]byte(addrStr))
	clientHMAC := mac.Sum(nil)
	fmt.Printf("Client HMAC: %s\n", hex.EncodeToString(clientHMAC))

	// Server verifies
	ok := VerifyHMAC(serverKey, clientHMAC, challenge, roomID, username, addr)
	fmt.Printf("Server verify: %v\n", ok)
	if !ok {
		t.Fatal("HMAC verification failed")
	}
}
