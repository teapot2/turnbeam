// Package limitbuf captures bounded subprocess output while draining excess data.
package limitbuf

type Buffer struct {
	Limit    int
	Overflow bool
	data     []byte
}

func (b *Buffer) Bytes() []byte { return b.data }

func (b *Buffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := max(0, b.Limit-len(b.data))
	if n > remaining {
		p = p[:remaining]
		b.Overflow = true
	}
	b.data = append(b.data, p...)
	return n, nil
}
