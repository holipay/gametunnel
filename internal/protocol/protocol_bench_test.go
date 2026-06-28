package protocol

import "testing"

func BenchmarkEncodeChecked(b *testing.B) {
	payload := make([]byte, 600) // typical game packet
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		EncodeChecked(TypeData, payload)
	}
}

func BenchmarkDecodeChecked(b *testing.B) {
	payload := make([]byte, 600)
	encoded := EncodeChecked(TypeData, payload)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		DecodeChecked(encoded)
	}
}
