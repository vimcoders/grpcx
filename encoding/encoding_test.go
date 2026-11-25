package encoding

import (
	"testing"

	"github.com/vimcoders/grpcx/generated/api"
)

// TestGetCodecReturnsOneProtoCodec covers the accessor: every subtype resolves to
// the same proto codec, and it reports the package name.
func TestGetCodecReturnsOneProtoCodec(t *testing.T) {
	first := GetCodec(Name)
	second := GetCodec("something-else")
	if first.Name() != Name {
		t.Errorf("Name() = %q, want %q", first.Name(), Name)
	}
	if first != second {
		t.Error("GetCodec returned two different codecs")
	}
}

// TestMarshalUsesProtoForProtoMessages covers the protobuf branch, and proves the
// bytes can be read back.
func TestMarshalUsesProtoForProtoMessages(t *testing.T) {
	in := &api.EchoRequest{Message: "hi"}
	b, err := Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out api.EchoRequest
	if err := Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.Message != in.Message {
		t.Errorf("round trip = %q, want %q", out.Message, in.Message)
	}
}

// TestMarshalUsesJSONForPlainGoValues covers the fallback branch: anything that
// is not a proto.Message is encoded as JSON.
func TestMarshalUsesJSONForPlainGoValues(t *testing.T) {
	b, err := Marshal(map[string]string{"k": "v"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if got := string(b); got != `{"k":"v"}` {
		t.Errorf("Marshal(map) = %s, want JSON", got)
	}

	var out map[string]string
	if err := Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out["k"] != "v" {
		t.Errorf("round trip = %v, want k=v", out)
	}
}

// TestMarshalReportsAnUnencodableValue covers a value neither branch can encode.
func TestMarshalReportsAnUnencodableValue(t *testing.T) {
	if _, err := Marshal(func() {}); err == nil {
		t.Error("Marshal of a func returned no error")
	}
}

// TestUnmarshalRejectsGarbage covers a payload the proto codec cannot read.
func TestUnmarshalRejectsGarbage(t *testing.T) {
	var out api.EchoRequest
	if err := Unmarshal([]byte{0xff, 0xff, 0xff}, &out); err == nil {
		t.Error("Unmarshal of garbage returned no error")
	}
}
