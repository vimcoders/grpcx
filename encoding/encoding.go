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

	"google.golang.org/grpc/encoding"
	"google.golang.org/protobuf/proto"
)

// Name is the name of the codec.
const Name = "proto"

// Codec is an alias for encoding.Codec.
type Codec encoding.Codec

// codec is the proto codec: protobuf for a proto.Message, encoding/json
// otherwise.
type codec struct{}

// Name implements [encoding.Codec].
func (c *codec) Name() string {
	return Name
}

// Marshal implements [encoding.Codec].
func (c codec) Marshal(v any) ([]byte, error) {
	switch vv := v.(type) {
	case proto.Message:
		return proto.Marshal(vv)
	default:
		return json.Marshal(v)
	}
}

// Unmarshal implements [encoding.Codec].
func (c codec) Unmarshal(p []byte, v any) error {
	switch vv := v.(type) {
	case proto.Message:
		return proto.Unmarshal(p, vv)
	default:
		return json.Unmarshal(p, vv)
	}
}

var defaultCodec = &codec{}

// GetCodec returns the built-in codec. The subtype is ignored: grpcx has exactly
// one, so every name resolves to the same instance.
func GetCodec(contentSubtype string) encoding.Codec {
	return defaultCodec
}

// Marshal encodes msg with the built-in codec. The transport does not route the
// call header through it: the header is marshalled by the client and server
// Codecs instead. It remains as the built-in entry point for a caller that
// wants that encoding explicitly.
func Marshal(msg any) ([]byte, error) {
	codec := GetCodec(Name)
	return codec.Marshal(msg)
}

// Unmarshal decodes p into msg with the built-in codec. It is what the request
// header uses: decodeCallOptions reads the header frame through it, whatever
// codec the caller installed for message bodies.
func Unmarshal(p []byte, msg any) error {
	codec := GetCodec(Name)
	return codec.Unmarshal(p, msg)
}
