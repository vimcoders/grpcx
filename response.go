package grpcx

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"time"

	"google.golang.org/protobuf/proto"
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

type Handler struct {
	serverOption
	net.Conn
	timeout time.Time
	impl    any
}

func (x *Handler) Handle(ctx context.Context, req request) error {
	seq, cmd := req.seq, req.cmd
	if int(cmd) >= len(x.Methods) {
		var replay []string
		for i := 0; i < len(x.Methods); i++ {
			replay = append(replay, x.Methods[i].MethodName)
		}
		ping, err := json.Marshal(replay)
		if err != nil {
			return err
		}
		if err := x.SetWriteDeadline(x.timeout); err != nil {
			return err
		}
		w := response{seq: seq, cmd: cmd, b: ping}
		if _, err := w.WriteTo(x.Conn); err != nil {
			return err
		}
		return nil
	}
	reply, err := x.Methods[req.cmd].Handler(x.impl, ctx, req.dec, x.Unary)
	if err != nil {
		return err
	}
	b, err := proto.Marshal(reply.(proto.Message))
	if err != nil {
		return err
	}
	if err := x.SetWriteDeadline(x.timeout); err != nil {
		return err
	}
	w := response{seq: req.seq, cmd: req.cmd, b: b}
	if _, err := w.WriteTo(x.Conn); err != nil {
		return err
	}
	return nil
}
