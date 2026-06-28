package netutil

import "testing"

func benchLZ4Compress(b *testing.B, size int) {
	enc := NewLZ4Encoder()
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i % 251)
	}
	b.SetBytes(int64(size))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		enc.Compress(data)
	}
}

func BenchmarkLZ4Compress60(b *testing.B)   { benchLZ4Compress(b, 60) }
func BenchmarkLZ4Compress300(b *testing.B)  { benchLZ4Compress(b, 300) }
func BenchmarkLZ4Compress1500(b *testing.B) { benchLZ4Compress(b, 1500) }

func benchLZ4Decompress(b *testing.B, size int) {
	enc := NewLZ4Encoder()
	dec := NewLZ4Decoder()
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i % 251)
	}
	compressed := enc.Compress(data)
	if compressed == nil {
		b.Skip("compression did not help for this size")
	}
	b.SetBytes(int64(size))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dec.Decompress(compressed)
	}
}

func BenchmarkLZ4Decompress300(b *testing.B)  { benchLZ4Decompress(b, 300) }
func BenchmarkLZ4Decompress1500(b *testing.B) { benchLZ4Decompress(b, 1500) }
