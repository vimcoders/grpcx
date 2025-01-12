package grpcx

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime/debug"
	"time"

	"github.com/vimcoders/grpcx/log"
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
	impl any
}

func (x *Handler) do(ctx context.Context, req request) (response, error) {
	seq, cmd := req.seq, req.cmd
	if int(cmd) >= len(x.Methods) {
		var replay []string
		for i := 0; i < len(x.Methods); i++ {
			replay = append(replay, x.Methods[i].MethodName)
		}
		ping, err := json.Marshal(replay)
		if err != nil {
			return response{}, err
		}
		return response{seq: seq, cmd: cmd, b: ping}, nil
	}
	reply, err := x.Methods[req.cmd].Handler(x.impl, ctx, req.dec, x.Unary)
	if err != nil {
		return response{}, err
	}
	b, err := proto.Marshal(reply.(proto.Message))
	if err != nil {
		return response{}, err
	}
	return response{seq: req.seq, cmd: req.cmd, b: b}, nil
}

func (x *Handler) Handle(ctx context.Context, c net.Conn) (err error) {
	defer func() {
		if e := recover(); e != nil {
			log.Error(e)
			debug.PrintStack()
		}
		if err != nil {
			log.Error(err)
		}
		if err := c.Close(); err != nil {
			log.Error(err)
		}
	}()
	buf := bufio.NewReaderSize(c, x.readBufferSize)
	for {
		select {
		case <-ctx.Done():
			return errors.New("shutdown")
		default:
		}
		if err := c.SetReadDeadline(time.Now().Add(x.timeout)); err != nil {
			return err
		}
		req, err := readRequest(buf)
		if err != nil {
			return err
		}
		response, err := x.do(ctx, req)
		if err != nil {
			return err
		}
		if err := c.SetWriteDeadline(time.Now().Add(x.timeout)); err != nil {
			return err
		}
		if _, err := response.WriteTo(c); err != nil {
			return err
		}
	}
}
