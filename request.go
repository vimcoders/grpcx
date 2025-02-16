package grpcx

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"

	"google.golang.org/protobuf/proto"
)

type request struct {
	seq     uint16
	cmd     uint16
	traceID [16]byte
	spanID  [8]byte
	payload []byte
}

func readRequest(buf *bufio.Reader) (request, error) {
	headerBytes, err := buf.Peek(2)
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
	traceID := [16]byte(b[6:22])
	spanID := [8]byte(b[22:30])
	seq := binary.BigEndian.Uint16(b[2:])
	cmd := binary.BigEndian.Uint16(b[4:])
	return request{
		seq:     seq,
		cmd:     cmd,
		traceID: traceID,
		spanID:  spanID,
		payload: b[30:],
	}, nil
}

func (x *request) WriteTo(w io.Writer) (int64, error) {
	var buf buffer
	if x.payload == nil {
		buf.WriteUint16(uint16(2+2+2+16+8), x.seq, x.cmd)
		buf.Write(x.traceID[:])
		buf.Write(x.spanID[:])
		return buf.WriteTo(w)
	}
	buf.WriteUint16(uint16(2+2+2+16+8+len(x.payload)), x.seq, x.cmd)
	buf.Write(x.traceID[:])
	buf.Write(x.spanID[:])
	if _, err := buf.Write(x.payload); err != nil {
		return 0, err
	}
	return buf.WriteTo(w)
}

func (x *request) dec(in any) error {
	if err := proto.Unmarshal(x.payload, in.(proto.Message)); err != nil {
		return err
	}
	return nil
}
