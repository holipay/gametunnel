package server

import "sync"

// pktPools is a set of graded byte buffer pools for incoming packets.
// Size classes: 512B (most game packets), 2KB, 16KB, 65535B (max UDP).
// This reduces memory waste: a 100-byte game packet no longer consumes a
// 65535-byte buffer.
var pktPools = [4]*sync.Pool{
	{New: func() interface{} { b := make([]byte, 512); return &b }},
	{New: func() interface{} { b := make([]byte, 2048); return &b }},
	{New: func() interface{} { b := make([]byte, 16384); return &b }},
	{New: func() interface{} { b := make([]byte, 65535); return &b }},
}

func pktPoolGet(n int) []byte {
	var idx int
	switch {
	case n <= 512:
		idx = 0
	case n <= 2048:
		idx = 1
	case n <= 16384:
		idx = 2
	default:
		idx = 3
	}
	bp := pktPools[idx].Get().(*[]byte)
	return (*bp)[:cap(*bp)]
}

func pktPoolPut(buf []byte) {
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
