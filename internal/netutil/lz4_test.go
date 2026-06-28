package netutil

import "testing"

func TestLZ4RoundTrip(t *testing.T) {
	enc := NewLZ4Encoder()
	dec := NewLZ4Decoder()

	tests := []struct {
		name string
		data []byte
	}{
		{"small_60", make([]byte, 60)},
		{"medium_300", make([]byte, 300)},
		{"large_1500", make([]byte, 1500)},
		{"repeating", make([]byte, 1600)},
		{"random", make([]byte, 500)},
	}

	// Fill repeating data
	for i := range tests[3].data {
		tests[3].data[i] = byte("ABCD"[i%4])
	}
	// Fill random-like data
	for i := range tests[4].data {
		tests[4].data[i] = byte(i % 251)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			compressed := enc.Compress(tt.data)
			if compressed == nil {
				// Small data may not compress — skip round-trip check
				return
			}

			decompressed, err := dec.Decompress(compressed)
			if err != nil {
				t.Fatalf("decompress error: %v", err)
			}
			dec.PutBuffer(decompressed)

			if len(decompressed) != len(tt.data) {
				t.Fatalf("length mismatch: got %d, want %d", len(decompressed), len(tt.data))
			}
			for i := range tt.data {
				if decompressed[i] != tt.data[i] {
					t.Fatalf("data mismatch at byte %d: got %d, want %d", i, decompressed[i], tt.data[i])
				}
			}
		})
	}
}

func TestLZ4CompressSkipsSmall(t *testing.T) {
	enc := NewLZ4Encoder()
	data := make([]byte, 30) // below MinCompressSize
	compressed := enc.Compress(data)
	if compressed != nil {
		t.Error("expected nil for data below MinCompressSize")
	}
}

func TestLZ4DecompressErrors(t *testing.T) {
	dec := NewLZ4Decoder()

	// Too short
	_, err := dec.Decompress([]byte{0})
	if err == nil {
		t.Error("expected error for short input")
	}

	// Zero original size
	_, err = dec.Decompress([]byte{0, 0, 1, 2, 3})
	if err == nil {
		t.Error("expected error for zero original size")
	}
}
