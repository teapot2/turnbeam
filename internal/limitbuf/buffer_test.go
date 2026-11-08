package limitbuf

import (
	"bytes"
	"testing"
)

func TestBuffer(t *testing.T) {
	for _, limit := range []int{0, 1, 64 << 10, 1 << 20} {
		b := Buffer{Limit: limit}
		data := bytes.Repeat([]byte("x"), limit)
		if n, err := b.Write(data); n != len(data) || err != nil || b.Overflow || !bytes.Equal(b.Bytes(), data) {
			t.Fatalf("limit %d: exact-limit write failed", limit)
		}
		for range 2 {
			if n, err := b.Write([]byte("more")); n != 4 || err != nil || !b.Overflow || !bytes.Equal(b.Bytes(), data) {
				t.Fatalf("limit %d: overflow was not drained and bounded", limit)
			}
		}
		b = Buffer{Limit: limit}
		if n, err := b.Write(append(data, 'y')); n != limit+1 || err != nil || !b.Overflow || !bytes.Equal(b.Bytes(), data) {
			t.Fatalf("limit %d: oversized write was not bounded", limit)
		}
	}
}
