package netutil

import "sync"

// pktPools is a graded byte buffer pool shared by FEC, LZ4, and client TUN reader.
// Size classes: 512B (most game packets), 2KB, 16KB, 65535B (max UDP).
var pktPools = [4]*sync.Pool{
	{New: func() interface{} { b := make([]byte, 512); return &b }},
	{New: func() interface{} { b := make([]byte, 2048); return &b }},
	{New: func() interface{} { b := make([]byte, 16384); return &b }},
	{New: func() interface{} { b := make([]byte, 65535); return &b }},
}

// PktBufGet returns a pooled buffer with capacity >= n.
func PktBufGet(n int) []byte {
	var idx int
	switch {
	case n <= 512:
		idx = 0
	case n <= 2048:
		idx = 1
	case n <= 16384:
		idx = 2
	case n <= 65535:
		idx = 3
	default:
		// n exceeds the largest pool class — allocate directly.
		// PktBufPut will skip returning this (no matching capacity), GC handles it.
		return make([]byte, n)
	}
	bp := pktPools[idx].Get().(*[]byte)
	return (*bp)[:cap(*bp)]
}

// PktBufPut returns a buffer to the pool. Only returns buffers whose
// capacity matches a known size class (safety guard).
func PktBufPut(buf []byte) {
	if buf == nil {
		return
	}
	c := cap(buf)
	switch c {
	case 512:
		pktPools[0].Put(&buf)
	case 2048:
		pktPools[1].Put(&buf)
	case 16384:
		pktPools[2].Put(&buf)
	case 65535:
		pktPools[3].Put(&buf)
	}
}
