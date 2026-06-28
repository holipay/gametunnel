package netutil

import "testing"

func benchFECEncode(b *testing.B, pktSize int) {
	enc := NewFECEncoder(DefaultFECGroupSize)
	data := make([]byte, pktSize)
	for i := range data {
		data[i] = byte(i % 251)
	}
	b.SetBytes(int64(pktSize))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		enc.Encode(data)
	}
}

func BenchmarkFECEncode64(b *testing.B)   { benchFECEncode(b, 64) }
func BenchmarkFECEncode300(b *testing.B)  { benchFECEncode(b, 300) }
func BenchmarkFECEncode1500(b *testing.B) { benchFECEncode(b, 1500) }

func benchFECDecode(b *testing.B, pktSize int) {
	dec := NewFECDecoder(DefaultFECGroupSize)
	defer dec.Close()
	parity := make([]byte, pktSize)
	for i := range parity {
		parity[i] = byte(i % 251)
	}
	b.SetBytes(int64(pktSize))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dec.ProcessParityPacket(uint32(i/DefaultFECGroupSize), DefaultFECGroupSize, parity)
	}
}

func BenchmarkFECDecode64(b *testing.B)   { benchFECDecode(b, 64) }
func BenchmarkFECDecode300(b *testing.B)  { benchFECDecode(b, 300) }
func BenchmarkFECDecode1500(b *testing.B) { benchFECDecode(b, 1500) }

func benchXORBytes(b *testing.B, size int) {
	dst := make([]byte, size)
	src := make([]byte, size)
	for i := range src {
		src[i] = byte(i % 251)
	}
	b.SetBytes(int64(size))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		xorBytes(dst, src)
	}
}

func BenchmarkXORBytes64(b *testing.B)   { benchXORBytes(b, 64) }
func BenchmarkXORBytes300(b *testing.B)  { benchXORBytes(b, 300) }
func BenchmarkXORBytes1500(b *testing.B) { benchXORBytes(b, 1500) }
