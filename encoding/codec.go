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

// GetCodec returns a new instance of the proto codec.
type codec struct{}

// Name implements [encoding.Codec].
func (c *codec) Name() string {
	return Name
}

// Marshal implements [encoding.Codec].
func (c codec) Marshal(msg any) ([]byte, error) {
	switch v := msg.(type) {
	case proto.Message:
		return proto.Marshal(v)
	default:
		return json.Marshal(v)
	}
}

// Unmarshal implements [encoding.Codec].
func (c codec) Unmarshal(p []byte, msg any) error {
	switch v := msg.(type) {
	case proto.Message:
		return proto.Unmarshal(p, v)
	default:
		return json.Unmarshal(p, v)
	}
}

var defaultCodec = &codec{}

// GetCodec returns a new instance of the proto codec.
func GetCodec(contentSubtype string) encoding.Codec {
	return defaultCodec
}
