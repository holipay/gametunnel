// Package hkdf implements the HMAC-based Key Derivation Function (HKDF)
// as defined in RFC 5869.
package hkdf

import (
	"crypto/hmac"
	"hash"
	"io"
)

// New returns an io.Reader from which callers can read the output of HKDF.
func New(hashFunc func() hash.Hash, secret, salt, info []byte) io.Reader {
	return &hkdf{hash: hashFunc, secret: secret, salt: salt, info: info}
}

type hkdf struct {
	hash   func() hash.Hash
	secret []byte
	salt   []byte
	info   []byte
	err    error
	buf    []byte
	block  uint8
}

func (h *hkdf) Read(p []byte) (int, error) {
	if h.err != nil {
		return 0, h.err
	}

	if len(h.buf) == 0 {
		h.block++
		block, err := h.derive(h.block)
		if err != nil {
			h.err = err
			return 0, err
		}
		h.buf = block
	}

	copied := copy(p, h.buf)
	h.buf = h.buf[copied:]

	return copied, nil
}

func (h *hkdf) derive(block byte) ([]byte, error) {
	mac := hmac.New(h.hash, h.salt)
	mac.Write(h.secret)
	mac.Write([]byte{block})

	prev := mac.Sum(nil)

	if len(h.info) > 0 {
		mac.Reset()
		mac.Write(prev)
		mac.Write(h.info)
		prev = mac.Sum(prev[:0])
	}

	return prev, nil
}
