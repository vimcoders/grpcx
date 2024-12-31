package grpcx

import (
	"encoding/binary"
	"io"
	"net"
	"sync"
)

const _MESSAGE_HEADER_LENGTH = 6
const _MESSAGE_HEADER = 2

var pool sync.Pool = sync.Pool{
	New: func() any {
		return &message{}
	},
}

var streams sync.Pool = sync.Pool{
	New: func() any {
		return &stream{
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

type stream struct {
	seq    uint16
	signal chan message
}

func (x *stream) invoke(iMessage message) error {
	x.signal <- iMessage
	return nil
}

var _ net.Addr = &Addr{}

type Addr struct {
	network string
	address string
}

func NewAddr(network, address string) net.Addr {
	return &Addr{network, address}
}

func (na *Addr) Network() string {
	return na.network
}

func (na *Addr) String() string {
	return na.address
}
