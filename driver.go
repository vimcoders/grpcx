package grpcx

import (
	"encoding/binary"
	"io"
	"math"
	"sync"
)

const _MESSAGE_HEADER_LENGTH = 6
const _MESSAGE_HEADER = 2

var pool sync.Pool = sync.Pool{
	New: func() any {
		return &message{}
	},
}

var _invoke_seq uint16
var _invoke_mutex sync.Mutex

var invoke sync.Pool = sync.Pool{
	New: func() any {
		_invoke_mutex.Lock()
		defer _invoke_mutex.Unlock()
		seq := _invoke_seq + 1
		_invoke_seq = seq % math.MaxUint16
		return &invoker{
			seq:    seq,
			signal: make(chan message, 1),
		}
	},
}

type message []byte

func (x message) seq() uint16 {
	return binary.BigEndian.Uint16(x[2:]) //2
}

func (x message) methodID() uint16 {
	return binary.BigEndian.Uint16(x[4:]) // 4
}

func (x message) body() []byte {
	return x[6:] // 6
}

func (x message) clone() message {
	b := pool.Get().(*message)
	b.Write(x)
	return *b
}

func (x *message) close() {
	if cap(*x) <= 0 {
		return
	}
	*x = (*x)[:0]
	pool.Put(x)
}

func (x *message) Write(p []byte) (int, error) {
	*x = append(*x, p...)
	return len(p), nil
}

func (x *message) WriteUint32(v uint32) {
	*x = binary.BigEndian.AppendUint32(*x, v)
}

func (x *message) WriteUint16(v uint16) {
	*x = binary.BigEndian.AppendUint16(*x, v)
}

func (x *message) WriteUint64(v uint64) {
	*x = binary.BigEndian.AppendUint64(*x, v)
}

func (x message) WriteTo(w io.Writer) (n int64, err error) {
	if nBytes := len(x); nBytes > 0 {
		m, e := w.Write(x)
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
	x.close()
	return n, nil
}

type invoker struct {
	seq    uint16
	signal chan message
}

func (x *invoker) invoke(iMessage message) error {
	x.signal <- iMessage
	return nil
}
