package transport

import (
	"testing"

	"github.com/vimcoders/grpcx/encoding"
	"github.com/vimcoders/grpcx/generated/api"

	"google.golang.org/protobuf/proto"
)

// protoCodec is the built-in codec every test below configures.
func protoCodec() encoding.Codec {
	return encoding.GetCodec(encoding.Name)
}

// TestDefaultCodecMarshalsProtoIntoPool covers the proto branch: the bytes come
// from the frame pool, which is what lets Free return them.
func TestDefaultCodecMarshalsProtoIntoPool(t *testing.T) {
	c := defaultCodec{ecodec: protoCodec()}
	buf, err := c.Marshal(&api.EchoRequest{Message: "hi"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	p, ok := buf.(defaultBuffer)
	if !ok {
		t.Fatalf("Marshal returned %T, want defaultBuffer", buf)
	}
	var out api.EchoRequest
	if err := proto.Unmarshal(p.ReadOnlyData(), &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.Message != "hi" {
		t.Errorf("payload decodes to %q, want hi", out.Message)
	}
	buf.Free()
}

// TestDefaultCodecMarshalsNonProtoThroughEncoding covers the fallback branch: a
// value that is not a proto.Message goes to ecodec and comes back unpooled.
func TestDefaultCodecMarshalsNonProtoThroughEncoding(t *testing.T) {
	c := defaultCodec{ecodec: protoCodec()}
	buf, err := c.Marshal(map[string]string{"k": "v"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	e, ok := buf.(emptyBuffer)
	if !ok {
		t.Fatalf("Marshal returned %T, want emptyBuffer", buf)
	}
	if got, want := string(e.ReadOnlyData()), `{"k":"v"}`; got != want {
		t.Errorf("payload = %q, want %q", got, want)
	}
	buf.Free()
}

// TestDefaultCodecUnmarshalReadsTheBuffer covers both decode branches through
// the proto codec: proto bytes into a message, JSON bytes into a Go value.
func TestDefaultCodecUnmarshalReadsTheBuffer(t *testing.T) {
	c := defaultCodec{ecodec: protoCodec()}

	req := api.EchoRequest{Message: "hi"}
	in := emptyBuffer(mustMarshal(t, &req))
	var out api.EchoRequest
	if err := c.Unmarshal(in, &out); err != nil {
		t.Fatalf("Unmarshal proto: %v", err)
	}
	if out.Message != "hi" {
		t.Errorf("proto message = %q, want hi", out.Message)
	}

	js := emptyBuffer([]byte(`{"k":"v"}`))
	var m map[string]string
	if err := c.Unmarshal(js, &m); err != nil {
		t.Fatalf("Unmarshal json: %v", err)
	}
	if m["k"] != "v" {
		t.Errorf("json value = %v, want k=v", m)
	}
}

// TestDefaultCodecKeepsProtoRoundTripSymmetric covers a custom ecodec: proto
// values stay on protobuf for both Marshal and Unmarshal, rather than being
// encoded by protobuf and then handed to the custom codec for decoding.
func TestDefaultCodecKeepsProtoRoundTripSymmetric(t *testing.T) {
	c := defaultCodec{ecodec: fixedCodec{out: []byte("M")}}
	buf, err := c.Marshal(&api.EchoRequest{Message: "hi"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	defer buf.Free()
	var out api.EchoRequest
	if err := c.Unmarshal(buf, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.Message != "hi" {
		t.Errorf("round trip = %q, want hi", out.Message)
	}
}

// TestDefaultCodecReportsMarshalError covers the fallback branch's error path:
// the result is a non-nil Buffer interface holding a nil emptyBuffer, which is
// what the comment on defaultMarshal promises.
func TestDefaultCodecReportsMarshalError(t *testing.T) {
	c := defaultCodec{ecodec: protoCodec()}
	buf, err := c.Marshal(func() {})
	if err == nil {
		t.Fatal("Marshal of a func returned no error")
	}
	if buf == nil {
		t.Fatal("Marshal returned a nil Buffer interface with an error")
	}
	if _, ok := buf.(emptyBuffer); !ok {
		t.Fatalf("Marshal returned %T, want emptyBuffer", buf)
	}
}

// TestDefaultCodecRejectsGarbage covers a buffer the proto codec cannot read.
func TestDefaultCodecRejectsGarbage(t *testing.T) {
	c := defaultCodec{ecodec: protoCodec()}
	in := emptyBuffer([]byte{0xff, 0xff, 0xff})
	var req api.EchoRequest
	if err := c.Unmarshal(in, &req); err == nil {
		t.Error("Unmarshal of garbage returned no error")
	}
}

// TestCodecKeepsEveryValueOffThePool covers emptyCodec: a proto.Message is
// delegated too, so nothing it returns can be handed back to the pool.
func TestCodecKeepsEveryValueOffThePool(t *testing.T) {
	c := emptyCodec{ecodec: protoCodec()}
	buf, err := c.Marshal(&api.EchoRequest{Message: "hi"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if _, ok := buf.(defaultBuffer); ok {
		t.Fatal("codec returned a pooled buffer")
	}
	if _, ok := buf.(emptyBuffer); !ok {
		t.Fatalf("Marshal returned %T, want emptyBuffer", buf)
	}
	buf.Free()
}

// TestCodecRoundTripsThroughEncoding covers emptyCodec's decode path.
func TestCodecRoundTripsThroughEncoding(t *testing.T) {
	c := emptyCodec{ecodec: protoCodec()}
	buf, err := c.Marshal(&api.EchoRequest{Message: "hi"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	defer buf.Free()
	var out api.EchoRequest
	if err := c.Unmarshal(buf, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.Message != "hi" {
		t.Errorf("round trip = %q, want hi", out.Message)
	}
}

// TestEmptyBufferFreeIsANoop covers the contract that makes emptyBuffer usable
// as a Buffer: Free must not touch the bytes, because the pool never owned them.
func TestEmptyBufferFreeIsANoop(t *testing.T) {
	e := emptyBuffer([]byte("payload"))
	e.Free()
	if got, want := string(e.ReadOnlyData()), "payload"; got != want {
		t.Errorf("data after Free = %q, want %q", got, want)
	}
}

// TestFixedCodecReachesNonProto covers WithCodec's reach: defaultCodec hands a
// non-proto value to the installed codec.
func TestFixedCodecReachesNonProto(t *testing.T) {
	c := defaultCodec{ecodec: fixedCodec{out: []byte("M")}}
	buf, err := c.Marshal(map[string]string{"a": "1"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	defer buf.Free()
	if got, want := string(buf.ReadOnlyData()), "M"; got != want {
		t.Errorf("payload = %q, want %q", got, want)
	}
}
