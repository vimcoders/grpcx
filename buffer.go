package grpcx

import (
	"encoding/binary"
	"io"
	"sync"
)

type buffer struct {
	b []byte
}

func (x *buffer) IsZero() bool {
	return len(x.b) <= 0
}

func (x *buffer) Close() error {
	x.b = x.b[:0]
	buffers.Put(x)
	return nil
}

func (x *buffer) Write(p []byte) (int, error) {
	if len(p) <= 0 {
		return 0, nil
	}
	x.b = append(x.b, p...)
	return len(p), nil
}

func (x *buffer) WriteUint32(v ...uint32) {
	for i := 0; i < len(v); i++ {
		x.b = binary.BigEndian.AppendUint32(x.b, v[i])
	}
}

func (x *buffer) WriteUint16(v ...uint16) {
	for i := 0; i < len(v); i++ {
		x.b = binary.BigEndian.AppendUint16(x.b, v[i])
	}
}

func (x *buffer) WriteUint64(v ...uint64) {
	for i := 0; i < len(v); i++ {
		x.b = binary.BigEndian.AppendUint64(x.b, v[i])
	}
}

func (x *buffer) WriteTo(w io.Writer) (n int64, err error) {
	if nBytes := len(x.b); nBytes > 0 {
		m, e := w.Write(x.b)
		if m > nBytes {
			panic("bytes.Buffer.WriteTo: invalid Write count")
		}
		if e != nil {
			return n, e
		}
		if m != nBytes {
			return n, io.ErrShortWrite
		}
	}
	x.Close()
	return n, nil
}

func (x *buffer) Bytes() []byte {
	return x.b
}

func NewBuffer(b []byte) *buffer {
	return &buffer{b: b}
}

var buffers sync.Pool = sync.Pool{
	New: func() any {
		return &buffer{}
	},
}
