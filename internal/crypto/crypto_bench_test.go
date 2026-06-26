package crypto

import "testing"

func BenchmarkEncrypt(b *testing.B) {
	key := make([]byte, KeySize)
	cipher, _ := NewCipher(key, DirClientToServer)
	plaintext := make([]byte, 600)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cipher.Encrypt(plaintext)
	}
}

func BenchmarkEncryptTo(b *testing.B) {
	key := make([]byte, KeySize)
	cipher, _ := NewCipher(key, DirClientToServer)
	plaintext := make([]byte, 600)
	// Pre-allocate buffer: header(2) + srcIP(4) + dstIP(4) + encrypted + CRC(4)
	buf := make([]byte, 0, 2+8+Overhead+len(plaintext)+16+4)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf = buf[:10] // reset to header+IPs position (2+8)
		cipher.EncryptTo(buf, plaintext)
	}
}

func BenchmarkDecrypt(b *testing.B) {
	key := make([]byte, KeySize)
	cipher, _ := NewCipher(key, DirClientToServer)
	plaintext := make([]byte, 600)
	encrypted := cipher.Encrypt(plaintext)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cipher.Decrypt(encrypted)
	}
}
