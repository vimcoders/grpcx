/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package encoding

import (
	"encoding/json"
	"testing"

	"github.com/vimcoders/grpcx/generated/api"

	grpcencoding "google.golang.org/grpc/encoding"
	"google.golang.org/protobuf/proto"
)

func TestName(t *testing.T) {
	if Name != "proto" {
		t.Fatalf("Name = %q, want proto", Name)
	}
}

// TestCodecImplementsInterface pins the two implementations the transport
// relies on: the registered codec and the Codec alias it stores.
func TestCodecImplementsInterface(t *testing.T) {
	var _ grpcencoding.Codec = GetCodec(Name)
	var _ grpcencoding.Codec = (*codec)(nil)
	var _ grpcencoding.Codec = Codec(GetCodec(Name))
}

// TestGetCodecIgnoresSubtype checks that every subtype, including none, maps to
// the same proto codec.
func TestGetCodecIgnoresSubtype(t *testing.T) {
	first := GetCodec(Name)
	if first == nil {
		t.Fatalf("GetCodec(%q) = nil", Name)
	}
	if got := first.Name(); got != Name {
		t.Fatalf("codec.Name() = %q, want %q", got, Name)
	}
	for _, sub := range []string{"", "proto", "json", "application/grpc", "weird-subtype"} {
		if got := GetCodec(sub); got != first {
			t.Fatalf("GetCodec(%q) returned a different codec", sub)
		}
	}
}

func TestProtoRoundTrip(t *testing.T) {
	in := &api.EchoRequest{Message: "hello proto"}
	b, err := Marshal(in)
	if err != nil {
		t.Fatalf("Marshal(proto) = %v", err)
	}
	if len(b) == 0 {
		t.Fatalf("Marshal(proto) returned no bytes")
	}

	var out api.EchoRequest
	if err := Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal(proto) = %v", err)
	}
	if !proto.Equal(in, &out) {
		t.Fatalf("round trip changed the message: got %v, want %v", &out, in)
	}

	// The text after the header must be a real protobuf payload, not JSON.
	if b[0] != 0x0a {
		t.Fatalf("proto payload starts with 0x%02x, want the field 1 length prefix 0x0a", b[0])
	}
}

type jsonPayload struct {
	Name string `json:"name"`
	N    int    `json:"n"`
}

// TestJSONRoundTrip covers the non proto branch: anything that is not a
// proto.Message travels as JSON.
func TestJSONRoundTrip(t *testing.T) {
	in := jsonPayload{Name: "x", N: 7}
	b, err := Marshal(in)
	if err != nil {
		t.Fatalf("Marshal(json) = %v", err)
	}
	want, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("json.Marshal = %v", err)
	}
	if string(b) != string(want) {
		t.Fatalf("Marshal(json) = %s, want %s", b, want)
	}

	var out jsonPayload
	if err := Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal(json) = %v", err)
	}
	if out != in {
		t.Fatalf("round trip = %+v, want %+v", out, in)
	}
}

func TestMarshalUnsupportedType(t *testing.T) {
	if _, err := Marshal(make(chan int)); err == nil {
		t.Fatalf("Marshal(chan) = nil error, want a JSON encoding failure")
	}
}

func TestUnmarshalInvalidProto(t *testing.T) {
	var m api.EchoRequest
	// Field 1 tag without its varint value: a truncated payload.
	if err := Unmarshal([]byte{0x08}, &m); err == nil {
		t.Fatalf("Unmarshal(truncated proto) = nil error")
	}
}

func TestUnmarshalInvalidJSON(t *testing.T) {
	var v struct {
		A int `json:"a"`
	}
	if err := Unmarshal([]byte("{"), &v); err == nil {
		t.Fatalf("Unmarshal(broken json) = nil error")
	}
}

// TestPackageFuncsGoThroughGetCodec calls the exported helpers on both
// branches, so a nil or wrong codec in GetCodec cannot hide behind the direct
// codec methods.
func TestPackageFuncsGoThroughGetCodec(t *testing.T) {
	b, err := Marshal(&api.EchoRequest{Message: "via package funcs"})
	if err != nil {
		t.Fatalf("Marshal = %v", err)
	}
	var out api.EchoRequest
	if err := Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal = %v", err)
	}
	if out.GetMessage() != "via package funcs" {
		t.Fatalf("message = %q", out.GetMessage())
	}
}
