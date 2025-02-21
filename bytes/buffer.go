package bytes

import (
	"encoding/binary"
	"fmt"
	"io"
)

type Buffer struct {
	b []byte
}

func (x *Buffer) Close() error {
	x.b = x.b[:0]
	return nil
}

func (x *Buffer) WriteString(s string) {
	if len(s) <= 0 {
		return
	}
	x.b = append(x.b, s...)
}

func (x *Buffer) Write(b []byte) (int, error) {
	if len(b) <= 0 {
		return 0, nil
	}
	x.b = append(x.b, b...)
	return len(b), nil
}

func (x *Buffer) WriteUint32(v ...uint32) {
	for i := 0; i < len(v); i++ {
		x.b = binary.BigEndian.AppendUint32(x.b, v[i])
	}
}

func (x *Buffer) WriteUint16(v ...uint16) {
	for i := 0; i < len(v); i++ {
		x.b = binary.BigEndian.AppendUint16(x.b, v[i])
	}
}

func (x *Buffer) WriteUint64(v ...uint64) {
	for i := 0; i < len(v); i++ {
		x.b = binary.BigEndian.AppendUint64(x.b, v[i])
	}
}

func (x *Buffer) Appendln(a ...any) {
	x.b = fmt.Appendln(x.b, a...)
}

func (x *Buffer) WriteTo(w io.Writer) (n int64, err error) {
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

func (x *Buffer) Bytes() []byte {
	return x.b
}
