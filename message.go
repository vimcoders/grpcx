package grpcx

import (
	"encoding/binary"
	"io"
	"sync"
)

type Message []byte

var pool sync.Pool = sync.Pool{
	New: func() any {
		return &Message{}
	},
}

func (x Message) seq() uint32 {
	return binary.BigEndian.Uint32(x[2:]) //2
}

func (x Message) methodID() uint16 {
	return binary.BigEndian.Uint16(x[6:])
}

func (x Message) openTracing() OpenTracing {
	high := binary.BigEndian.Uint64(x[8:])
	low := binary.BigEndian.Uint64(x[16:])
	spanID := binary.BigEndian.Uint64(x[24:])
	return OpenTracing{High: high, Low: low, SpanID: spanID}
}

func (x Message) clone() Message {
	//b := pool.Get().(*Message)
	var b Message
	b.Write(x)
	return b
}

func (x *Message) reset() {
	if cap(*x) <= 0 {
		return
	}
	*x = (*x)[:0]
	pool.Put(x)
}

func (x *Message) Write(p []byte) (int, error) {
	*x = append(*x, p...)
	return len(p), nil
}

func (x *Message) WriteUint32(v uint32) {
	*x = binary.BigEndian.AppendUint32(*x, v)
}

func (x *Message) WriteUint16(v uint16) {
	*x = binary.BigEndian.AppendUint16(*x, v)
}

func (x *Message) WriteUint64(v uint64) {
	*x = binary.BigEndian.AppendUint64(*x, v)
}

// WriteTo writes data to w until the buffer is drained or an error occurs.
// The return value n is the number of bytes written; it always fits into an
// int, but it is int64 to match the io.WriterTo interface. Any error
// encountered during the write is also returned.
func (x Message) WriteTo(w io.Writer) (n int64, err error) {
	if nBytes := len(x); nBytes > 0 {
		m, e := w.Write(x)
		if m > nBytes {
			panic("bytes.Buffer.WriteTo: invalid Write count")
		}
		if e != nil {
			return n, e
		}
		// all bytes should have been written, by definition of
		// Write method in io.Writer
		if m != nBytes {
			return n, io.ErrShortWrite
		}
	}
	// Buffer is now empty; reset.
	//x.reset()
	return n, nil
}
