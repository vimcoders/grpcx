package log

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"sync"
	"time"
)

type buffer struct {
	b []byte
}

func NewBuffer(size int) *buffer {
	return &buffer{b: make([]byte, size)}
}

func (x *buffer) Close() error {
	x.b = x.b[:0]
	bufferFree.Put(x)
	return nil
}

func (x *buffer) Write(s string) {
	x.b = append(x.b, s...)
}

func (x *buffer) WriteUint32(values ...uint32) {
	for i := 0; i < len(values); i++ {
		x.b = binary.BigEndian.AppendUint32(x.b, values[i])
	}
}

func (x *buffer) Appendln(a ...any) {
	x.b = fmt.Appendln(x.b, a...)
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

func newPrinter(prefix string, a ...any) io.WriterTo {
	buffer := bufferFree.Get().(*buffer)
	buffer.Write(time.Now().Format("2006-01-02 15:04:05"))
	buffer.Write(prefix)
	buffer.Appendln(a...)
	return buffer
}

// func newPrinterf(prefix, format string, a ...any) io.WriterTo {
// 	return newPrinter(prefix, fmt.Sprintf(format, a...))
// }

var bufferFree sync.Pool = sync.Pool{
	New: func() any {
		return &buffer{}
	},
}

type Handler interface {
	Handle(context.Context, []byte) error
	Close() error
}
