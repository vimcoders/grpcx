package grpcx

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"math"

	"google.golang.org/protobuf/proto"
)

type request struct {
	seq uint16
	cmd uint16
	b   []byte
}

func NewRequest(cmd uint16, req any) (request, error) {
	b, err := proto.Marshal(req.(proto.Message))
	if err != nil {
		return request{}, err
	}
	return request{
		cmd: cmd,
		b:   b,
	}, nil
}

func NewPingRequest() request {
	return request{
		cmd: math.MaxUint16,
	}
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
		seq: seq,
		cmd: cmd,
		b:   b[6:],
	}, nil
}

func (x *request) WriteTo(w io.Writer) (int64, error) {
	buf := buffers.Get().(*buffer)
	if x.b == nil {
		buf.WriteUint16(uint16(2+2+2), x.seq, x.cmd)
		return buf.WriteTo(w)
	}
	buf.WriteUint16(uint16(2+2+2+len(x.b)), x.seq, x.cmd)
	if _, err := buf.Write(x.b); err != nil {
		return 0, err
	}
	return buf.WriteTo(w)
}

func (x *request) dec(in any) error {
	if err := proto.Unmarshal(x.b, in.(proto.Message)); err != nil {
		return err
	}
	return nil
}
