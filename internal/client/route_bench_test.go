package client

import (
	"net"
	"testing"

	"github.com/holipay/gametunnel/internal/crypto"
)

func BenchmarkBuildDataPacket(b *testing.B) {
	srcIP := net.IPv4(10, 0, 0, 2).To4()
	dstIP := net.IPv4(10, 0, 0, 3).To4()
	data := make([]byte, 600)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buildDataPacket(srcIP, dstIP, data, 0)
	}
}

func BenchmarkBuildEncryptedDataPacket(b *testing.B) {
	srcIP := net.IPv4(10, 0, 0, 2).To4()
	dstIP := net.IPv4(10, 0, 0, 3).To4()
	data := make([]byte, 600)
	key := make([]byte, 32)
	cipher, _ := crypto.NewCipher(key, crypto.DirClientToServer)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buildEncryptedDataPacket(srcIP, dstIP, data, cipher, 0)
	}
}

func BenchmarkEncryptThenBuild(b *testing.B) {
	srcIP := net.IPv4(10, 0, 0, 2).To4()
	dstIP := net.IPv4(10, 0, 0, 3).To4()
	data := make([]byte, 600)
	key := make([]byte, 32)
	cipher, _ := crypto.NewCipher(key, crypto.DirClientToServer)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		encrypted := cipher.Encrypt(data)
		buildDataPacket(srcIP, dstIP, encrypted, 0)
	}
}

func BenchmarkIpKey(b *testing.B) {
	ip := net.IPv4(10, 0, 0, 3).To4()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ipKey(ip)
	}
}
