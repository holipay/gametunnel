package netutil

import (
	"encoding/binary"
	"sync"
)

// LZ4 compression for GameTunnel data packets.
//
// Uses a simplified LZ4-like algorithm optimized for small packets (60-1500 bytes).
// For packets < 64 bytes, compression is skipped (overhead exceeds benefit).
//
// The compression is intentionally lightweight — gaming traffic needs
// sub-microsecond compression latency, not maximum ratio.
//
// Wire format of compressed payload:
//
//	[2B: original_size (little-endian)] [compressed_data]
//
// If original_size == 0, the data is uncompressed (passthrough).

const (
	// MinCompressSize is the minimum payload size to attempt compression.
	// Below this, the overhead exceeds the benefit.
	MinCompressSize = 64

	// CompressFlag is set in the DataPayload flags byte when data is compressed.
	CompressFlag byte = 0x01
)

// ── Encoder ────────────────────────────────────────────────────

// LZ4Encoder compresses data packets.
// Uses a sync.Pool to reuse compression buffers.
type LZ4Encoder struct {
	pool sync.Pool
}

// NewLZ4Encoder creates a new encoder.
func NewLZ4Encoder() *LZ4Encoder {
	return &LZ4Encoder{
		pool: sync.Pool{
			New: func() interface{} {
				buf := make([]byte, 0, 2048)
				return &buf
			},
		},
	}
}

// Compress compresses data using a fast LZ4-like algorithm.
// Returns compressed data with a 2-byte original-size header.
// Returns nil if data is too small to compress or compression doesn't help.
func (e *LZ4Encoder) Compress(data []byte) []byte {
	if len(data) < MinCompressSize {
		return nil
	}

	compressed := e.lz4Compress(data)
	if compressed == nil || len(compressed)+2 >= len(data) {
		return nil // compression didn't help
	}

	// Build result: [2B original_size][compressed_data]
	result := make([]byte, 2+len(compressed))
	binary.LittleEndian.PutUint16(result[0:2], uint16(len(data)))
	copy(result[2:], compressed)
	return result
}

// lz4Compress performs the actual LZ4 block compression.
// Returns nil if the compressed output would be larger than input.
func (e *LZ4Encoder) lz4Compress(src []byte) []byte {
	if len(src) == 0 {
		return nil
	}

	dst := e.pool.Get().(*[]byte)
	buf := (*dst)[:0]
	defer func() {
		*dst = buf
		e.pool.Put(dst)
	}()

	// LZ4 block format:
	// For each literal run + match:
	//   [token: 4 bits literal_len + 4 bits match_len]
	//   [literal_len bytes of literals (if > 15, extend)]
	//   [2 bytes: offset]
	//   [match_len bytes (if > 15, extend)]

	const (
		minMatch  = 4    // minimum match length
		hashSize = 4096 // hash table size
	)

	var hashTable [hashSize]int
	for i := range hashTable {
		hashTable[i] = -1
	}

	srcLen := len(src)
	ip := 0 // input position
	anchor := 0

	// Reserve space for the token stream
	// We write literals and matches inline

	for ip < srcLen {
		// Find match
		if ip+minMatch > srcLen {
			break
		}

		// Hash 4 bytes
		h := hash4(src[ip:]) % hashSize
		match := hashTable[h]
		hashTable[h] = ip

		if match < 0 || match >= ip || ip-match > 0xFFFF {
			ip++
			continue
		}

		// Verify match
		matchLen := 0
		for match+matchLen < ip && ip+matchLen < srcLen && src[match+matchLen] == src[ip+matchLen] {
			matchLen++
		}

		if matchLen < minMatch {
			ip++
			continue
		}

		// Emit literal run [anchor, ip)
		litLen := ip - anchor
		offset := ip - match

		// Check if output would exceed input
		if len(buf)+1+litLen+2+matchLen-minMatch >= srcLen {
			return nil // compression doesn't help
		}

		// Token: 4 bits literal length + 4 bits match length
		tokenLit := litLen
		if tokenLit > 15 {
			tokenLit = 15
		}
		tokenMatch := matchLen - minMatch
		if tokenMatch > 15 {
			tokenMatch = 15
		}
		buf = append(buf, byte(tokenLit<<4|tokenMatch))

		// Extended literal length (LZ4 spec: each 0xFF adds 255, final byte is remainder)
		// Only emit extension bytes when the 4-bit value in the token is exactly 15.
		if tokenLit == 15 {
			remaining := litLen - 15
			for remaining >= 255 {
				buf = append(buf, 0xFF)
				remaining -= 255
			}
			buf = append(buf, byte(remaining))
		}

		// Literals
		buf = append(buf, src[anchor:ip]...)

		// Offset (little-endian)
		buf = append(buf, byte(offset), byte(offset>>8))

		// Extended match length (LZ4 spec: each 0xFF adds 255, final byte is remainder)
		// Only emit extension bytes when the 4-bit value in the token is exactly 15.
		if tokenMatch == 15 {
			extRemaining := matchLen - minMatch - 15
			for extRemaining >= 255 {
				buf = append(buf, 0xFF)
				extRemaining -= 255
			}
			buf = append(buf, byte(extRemaining))
		}

		ip += matchLen
		anchor = ip
	}

	// Emit remaining literals [anchor, srcLen)
	litLen := srcLen - anchor
	if len(buf)+1+litLen >= srcLen {
		return nil
	}

	tokenLit := litLen
	if tokenLit > 15 {
		tokenLit = 15
	}
	buf = append(buf, byte(tokenLit<<4))
	if tokenLit == 15 {
		remaining := litLen - 15
		for remaining >= 255 {
			buf = append(buf, 0xFF)
			remaining -= 255
		}
		buf = append(buf, byte(remaining))
	}
	buf = append(buf, src[anchor:srcLen]...)

	return buf
}

func hash4(b []byte) int {
	if len(b) < 4 {
		return 0
	}
	return int(uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24)
}

// ── Decoder ────────────────────────────────────────────────────

// LZ4Decoder decompresses LZ4-compressed data packets.
type LZ4Decoder struct{}

// NewLZ4Decoder creates a new decoder.
func NewLZ4Decoder() *LZ4Decoder {
	return &LZ4Decoder{}
}

// Decompress decompresses data that was compressed by LZ4Encoder.
// The input must include the 2-byte original-size header.
// Returns the original uncompressed data.
func (d *LZ4Decoder) Decompress(data []byte) ([]byte, error) {
	if len(data) < 2 {
		return nil, &FECError{"compressed data too short"}
	}

	origSize := int(binary.LittleEndian.Uint16(data[0:2]))
	if origSize == 0 {
		return nil, &FECError{"invalid compressed data: zero original size"}
	}

	compressed := data[2:]
	result := make([]byte, origSize)

	n := d.lz4Decompress(compressed, result)
	if n != origSize {
		return nil, &FECError{"decompressed size mismatch"}
	}

	return result, nil
}

// lz4Decompress performs LZ4 block decompression.
func (d *LZ4Decoder) lz4Decompress(src, dst []byte) int {
	srcLen := len(src)
	dstLen := len(dst)
	si := 0 // source index
	di := 0 // destination index

	for si < srcLen {
		// Read token
		token := int(src[si])
		si++

		// Literal run
		litLen := token >> 4
		if litLen == 15 {
			for si < srcLen {
				b := int(src[si])
				si++
				litLen += b
				if b != 0xFF {
					break
				}
			}
		}

		// Copy literals
		for i := 0; i < litLen && si < srcLen && di < dstLen; i++ {
			dst[di] = src[si]
			si++
			di++
		}

		if si >= srcLen || di >= dstLen {
			break
		}

		// Read offset
		if si+2 > srcLen {
			break
		}
		offset := int(src[si]) | int(src[si+1])<<8
		si += 2

		if offset == 0 || di-offset < 0 {
			break
		}

		// Match run
		matchLen := (token & 0x0F) + 4 // minMatch = 4
		if matchLen-4 == 15 {
			for si < srcLen {
				b := int(src[si])
				si++
				matchLen += b
				if b != 0xFF {
					break
				}
			}
		}

		// Copy match (may overlap — must copy byte by byte)
		matchStart := di - offset
		for i := 0; i < matchLen && di < dstLen && matchStart+i < dstLen; i++ {
			dst[di] = dst[matchStart+i]
			di++
		}
	}

	return di
}

// IsCompressed checks if data has the compression flag set.
func IsCompressed(flags byte) bool {
	return flags&CompressFlag != 0
}
