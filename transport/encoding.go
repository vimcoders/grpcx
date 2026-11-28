package transport

import (
	"github.com/vimcoders/grpcx/encoding"
	"google.golang.org/protobuf/proto"
)

// emptyBuffer is a Buffer over bytes the frame pool never owned. Free does
// nothing, so any encoding.Codec result can be handed out as a Buffer.
type emptyBuffer []byte

// Free does nothing: these bytes did not come from getmbuf.
func (e emptyBuffer) Free() {
}

// ReadOnlyData returns the underlying bytes as they are.
func (e emptyBuffer) ReadOnlyData() []byte {
	return e
}

// defaultCodec is the Codec installed while batching is off. It splits on the
// type of the value in both directions: a proto.Message is marshalled into a
// pool buffer and decoded with proto.Unmarshal, while every other value is
// delegated to ecodec. The split means a Buffer returned by Marshal is not
// always pooled, and Free is only meaningful on the proto branch.
type defaultCodec struct {
	ecodec encoding.Codec
}

// Marshal implements Codec. For a proto.Message it sizes the buffer with
// proto.Size and calls MarshalAppend with UseCachedSize; the returned slice is
// discarded and the pooled buffer is returned with its original length. For
// every other type it delegates to defaultMarshal.
func (c defaultCodec) Marshal(v any) (Buffer, error) {
	switch vv := v.(type) {
	case proto.Message:
		n := proto.Size(vv)
		p := getmbuf(n)
		marshalOptions := proto.MarshalOptions{UseCachedSize: true}
		if _, err := marshalOptions.MarshalAppend(p[:0], vv); err != nil {
			p.Free()
			return nil, err
		}
		return p, nil
	default:
		return c.defaultMarshal(v)
	}
}

// defaultMarshal delegates to ecodec. The concrete result type is emptyBuffer,
// so the bytes are not returned to the pool by Free. A marshal error still
// carries the (nil) emptyBuffer as its value, so the result is a non-nil Buffer
// interface; the proto branch above returns a literal nil instead.
func (c defaultCodec) defaultMarshal(v any) (emptyBuffer, error) {
	return c.ecodec.Marshal(v)
}

// Unmarshal implements Codec symmetrically with Marshal: proto.Message values
// are decoded by protobuf, and every other value is handed to ecodec. The buffer
// is read, never freed.
func (c defaultCodec) Unmarshal(b Buffer, v any) error {
	if vv, ok := v.(proto.Message); ok {
		return proto.Unmarshal(b.ReadOnlyData(), vv)
	}
	return c.ecodec.Unmarshal(b.ReadOnlyData(), v)
}

// emptyCodec adapts any encoding.Codec to Codec without special-casing proto.
// Every value, proto.Message included, goes through ecodec, and the result is
// always an emptyBuffer. Options.newCodec installs it while batching is on, so a
// frame queued in the pending batch is never handed back to the pool before the
// flush has written it.
type emptyCodec struct {
	ecodec encoding.Codec
}

// Marshal implements Codec by delegating to defaultMarshal.
func (c emptyCodec) Marshal(v any) (Buffer, error) {
	return c.defaultMarshal(v)
}

// defaultMarshal delegates to ecodec and returns the bytes as an emptyBuffer.
// As with defaultCodec, an error result still carries a non-nil Buffer
// interface holding a nil emptyBuffer.
func (c emptyCodec) defaultMarshal(v any) (emptyBuffer, error) {
	return c.ecodec.Marshal(v)
}

// Unmarshal implements Codec: it hands the buffer's bytes to ecodec and leaves
// the buffer's ownership with the caller.
func (c emptyCodec) Unmarshal(b Buffer, v any) error {
	return c.ecodec.Unmarshal(b.ReadOnlyData(), v)
}
