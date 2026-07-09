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

package metadata

import (
	"context"
	"fmt"
	"maps"
	"strings"
)

// MD is the user type for ttrpc metadata
type MD map[string]string

// Pairs returns an MD formed by the mapping of key, value ...
// Pairs panics if len(kv) is odd.
//
// Only the following ASCII characters are allowed in keys:
//   - digits: 0-9
//   - uppercase letters: A-Z (normalized to lower)
//   - lowercase letters: a-z
//   - special characters: -_.
//
// Uppercase letters are automatically converted to lowercase.
//
// Keys beginning with "grpc-" are reserved for grpc-internal use only and may
// result in errors if set in metadata.
func Pairs(kv ...string) MD {
	if len(kv)%2 == 1 {
		panic(fmt.Sprintf("metadata: Pairs got the odd number of input pairs for metadata: %d", len(kv)))
	}
	var md = make(MD)
	for i := 0; i < len(kv); i += 2 {
		md[kv[i]] = kv[i+1]
	}
	return md
}

// Get returns the metadata for a given key when they exist.
// If there is no metadata, a nil slice and false are returned.
func (m MD) Get(key string) (string, bool) {
	key = strings.ToLower(key)
	if v, ok := m[key]; ok {
		return v, true
	}
	return "", false
}

// Clone returns a copy of MD or nil if it's nil.
// It's copied from golang's `http.Header.Clone` implementation:
// https://cs.opensource.google/go/go/+/refs/tags/go1.23.4:src/net/http/header.go;l=94
func (m MD) Clone() MD {
	if m == nil {
		return nil
	}

	clone := make(MD, len(m))
	maps.Copy(clone, m)
	return clone
}

type metadataKey struct{}

// GetMetadata retrieves metadata from context.Context (previously attached with WithMetadata)
func GetMetadata(ctx context.Context) (MD, bool) {
	metadata, ok := ctx.Value(metadataKey{}).(MD)
	return metadata, ok
}

// GetMetadataValue gets a specific metadata value by name from context.Context
func GetMetadataValue(ctx context.Context, name string) (string, bool) {
	metadata, ok := GetMetadata(ctx)
	if !ok {
		return "", false
	}
	return metadata.Get(name)
}

// WithMetadata attaches metadata map to a context.Context
func WithMetadata(ctx context.Context, md MD) context.Context {
	return context.WithValue(ctx, metadataKey{}, md)
}

// AppendToContext returns a new context with the provided kv merged
// with any existing metadata in the context. Please refer to the documentation
// of Pairs for a description of kv.
func AppendToContext(ctx context.Context, kv ...string) context.Context {
	if len(kv)%2 == 1 {
		panic(fmt.Sprintf("metadata: AppendToContext got an odd number of input pairs for metadata: %d", len(kv)))
	}
	md, ok := GetMetadata(ctx)
	if !ok {
		return WithMetadata(ctx, Pairs(kv...))
	}
	for i := 0; i < len(kv); i += 2 {
		md[kv[i]] = kv[i+1]
	}
	return WithMetadata(ctx, md)
}
