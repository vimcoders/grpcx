package grpcx

import (
	"encoding/binary"
	"io"
	"net"
	"sync"
)

const _MESSAGE_HEADER_LENGTH = 6
const _MESSAGE_HEADER = 2

var buffers sync.Pool = sync.Pool{
	New: func() any {
		return &buffer{}
	},
}

var requests sync.Pool = sync.Pool{
	New: func() any {
		return &request{
			ch: make(chan *buffer, 1),
		}
	},
}

type buffer struct {
	b []byte
}

func NewBuffer(b []byte) *buffer {
	return &buffer{b: b}
}

func (x buffer) seq() uint16 {
	return binary.BigEndian.Uint16(x.b[2:]) //2
}

func (x buffer) methodID() uint16 {
	return binary.BigEndian.Uint16(x.b[4:]) // 4
}

func (x buffer) body() []byte {
	return x.b[6:] // 6
}

func (x *buffer) close() {
	x.b = x.b[:0]
	buffers.Put(x)
}

func (x *buffer) Write(p []byte) (int, error) {
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

func (x buffer) WriteTo(w io.Writer) (n int64, err error) {
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
	x.close()
	return n, nil
}

type request struct {
	seq    uint16
	method uint16
	body   []byte
	ch     chan *buffer
}

func (x request) Size() uint16 {
	return uint16(2 + 2 + 2 + len(x.body))
}

func (x *request) WriteTo(c net.Conn) (int64, error) {
	buf := buffers.Get().(*buffer)
	buf.WriteUint16(x.Size(), x.seq, x.method)
	buf.Write(x.body)
	return buf.WriteTo(c)
}

var _ net.Addr = &Addr{}

type Addr struct {
	network string
	address string
}

func NewAddr(network, address string) net.Addr {
	return &Addr{network, address}
}

func (x *Addr) Network() string {
	return x.network
}

func (x *Addr) String() string {
	return x.address
}
