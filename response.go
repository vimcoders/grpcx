package grpcx

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/vimcoders/grpcx/bytes"
)

type response struct {
	seq     uint16
	cmd     uint16
	payload []byte
}

func (x *response) WriteTo(w io.Writer) (int64, error) {
	var buf bytes.Buffer
	if x.payload == nil {
		buf.WriteUint16(uint16(2+2+2), x.seq, x.cmd)
		return buf.WriteTo(w)
	}
	buf.WriteUint16(uint16(2+2+2+len(x.payload)), x.seq, x.cmd)
	if _, err := buf.Write(x.payload); err != nil {
		return 0, err
	}
	return buf.WriteTo(w)
}

func readResponse(buf *bufio.Reader) (response, error) {
	headerBytes, err := buf.Peek(2)
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
	var buffer bytes.Buffer
	if _, err := buffer.Write(b[6:]); err != nil {
		return response{}, err
	}
	return response{
		seq:     seq,
		cmd:     cmd,
		payload: buffer.Bytes(),
	}, nil
}
