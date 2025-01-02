package grpcx

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"runtime/debug"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/protobuf/proto"
)

const _MESSAGE_HEADER = 2

type ClientParameters = keepalive.ClientParameters
type UnaryServerInterceptor = grpc.UnaryServerInterceptor

var buffers sync.Pool = sync.Pool{
	New: func() any {
		return &buffer{}
	},
}

type buffer struct {
	b []byte
}

func readBuffer(buf *bufio.Reader) (*buffer, error) {
	headerBytes, err := buf.Peek(_MESSAGE_HEADER)
	if err != nil {
		return nil, err
	}
	length := int(binary.BigEndian.Uint16(headerBytes))
	if length > buf.Size() {
		return nil, fmt.Errorf("header %v too long", length)
	}
	iMessage, err := buf.Peek(length)
	if err != nil {
		return nil, err
	}
	if _, err := buf.Discard(len(iMessage)); err != nil {
		return nil, err
	}
	buffer := buffers.Get().(*buffer)
	if _, err := buffer.Write(iMessage); err != nil {
		return nil, err
	}
	return buffer, nil
}

func (x buffer) seq() uint16 {
	return binary.BigEndian.Uint16(x.b[2:]) //2
}

func (x buffer) cmd() uint16 {
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
	cmd  uint16
	body []byte
	ch   chan *buffer
}

func NewRequest(cmd uint16, req any) (*request, error) {
	b, err := proto.Marshal(req.(proto.Message))
	if err != nil {
		return nil, err
	}
	return &request{
		cmd:  cmd,
		body: b,
		ch:   make(chan *buffer, 1),
	}, nil
}

func (x *request) invoke(buf *buffer) error {
	if e := recover(); e != nil {
		fmt.Println(e)
		debug.PrintStack()
	}
	if len(x.ch) > 0 {
		return nil
	}
	x.ch <- buf
	return nil
}

func (x request) Size() uint16 {
	return uint16(2 + 2 + 2 + len(x.body))
}

func NewResponseWriter(seq, cmd uint16, reply any) (io.WriterTo, error) {
	b, err := proto.Marshal(reply.(proto.Message))
	if err != nil {
		return nil, err
	}
	buf := buffers.Get().(*buffer)
	buf.WriteUint16(uint16(2+2+2+len(b)), seq, cmd)
	buf.Write(b)
	return buf, nil
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
