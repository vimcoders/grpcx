package grpcx

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net"
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

func readRequest(buf *bufio.Reader) (request, error) {
	headerBytes, err := buf.Peek(_MESSAGE_HEADER)
	if err != nil {
		return request{}, err
	}
	length := int(binary.BigEndian.Uint16(headerBytes))
	if length > buf.Size() {
		return request{}, fmt.Errorf("header %v too long", length)
	}
	b, err := buf.Peek(length)
	if err != nil {
		return request{}, err
	}
	if _, err := buf.Discard(len(b)); err != nil {
		return request{}, err
	}
	seq := binary.BigEndian.Uint16(b[2:])
	cmd := binary.BigEndian.Uint16(b[4:])
	return request{
		seq:    seq,
		cmd:    cmd,
		buffer: NewBuffer(b[6:]),
	}, nil
}

func readResponse(buf *bufio.Reader) (response, error) {
	headerBytes, err := buf.Peek(_MESSAGE_HEADER)
	if err != nil {
		return response{}, err
	}
	length := int(binary.BigEndian.Uint16(headerBytes))
	if length > buf.Size() {
		return response{}, fmt.Errorf("header %v too long", length)
	}
	b, err := buf.Peek(length)
	if err != nil {
		return response{}, err
	}
	if _, err := buf.Discard(len(b)); err != nil {
		return response{}, err
	}
	seq := binary.BigEndian.Uint16(b[2:])
	cmd := binary.BigEndian.Uint16(b[4:])
	return response{
		seq:    seq,
		cmd:    cmd,
		buffer: NewBuffer(b[6:]),
	}, nil
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

func (x *buffer) Clone() *buffer {
	buf := buffers.Get().(*buffer)
	buf.Write(x.b)
	return buf
}

func NewBuffer(b []byte) *buffer {
	return &buffer{b: b}
}

type response struct {
	seq uint16
	cmd uint16
	*buffer
}

func (x *response) WriteTo(w io.Writer) (int64, error) {
	buf := buffers.Get().(*buffer)
	if x.buffer == nil {
		buf.WriteUint16(uint16(2+2+2), x.seq, x.cmd)
		return buf.WriteTo(w)
	}
	buf.WriteUint16(uint16(2+2+2+len(x.Bytes())), x.seq, x.cmd)
	if _, err := buf.Write(x.Bytes()); err != nil {
		return 0, err
	}
	return buf.WriteTo(w)
}

type request struct {
	seq uint16
	cmd uint16
	*buffer
}

func NewRequest(cmd uint16, req any) (*request, error) {
	b, err := proto.Marshal(req.(proto.Message))
	if err != nil {
		return nil, err
	}
	return &request{
		cmd:    cmd,
		buffer: NewBuffer(b),
	}, nil
}

func NewPingRequest() *request {
	return &request{
		cmd: math.MaxUint16,
	}
}

func (x *request) WriteTo(w io.Writer) (int64, error) {
	buf := buffers.Get().(*buffer)
	if x.buffer == nil {
		buf.WriteUint16(uint16(2+2+2), x.seq, x.cmd)
		return buf.WriteTo(w)
	}
	buf.WriteUint16(uint16(2+2+2+len(x.Bytes())), x.seq, x.cmd)
	if _, err := buf.Write(x.Bytes()); err != nil {
		return 0, err
	}
	return buf.WriteTo(w)
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
