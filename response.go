package grpcx

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
)

type response struct {
	seq uint16
	cmd uint16
	b   []byte
}

func (x *response) WriteTo(w io.Writer) (int64, error) {
	var buf buffer
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
	var buffer buffer
	if _, err := buffer.Write(b[6:]); err != nil {
		return response{}, err
	}
	return response{
		seq: seq,
		cmd: cmd,
		b:   buffer.Bytes(),
	}, nil
}
