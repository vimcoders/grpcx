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

package ttrpc

import (
	"context"
	"strings"
)

// MD is the user type for ttrpc metadata
type MD []string

// Get returns the metadata for a given key when they exist.
// If there is no metadata, a nil slice and false are returned.
func (m MD) Get(key string) (string, bool) {
	key = strings.ToLower(key)
	for i := 0; i < len(m); i += 2 {
		if strings.ToLower(m[i]) == key {
			return m[i+1], true
		}
	}
	return "", false
}

// Append appends additional values to the given key.
func (m MD) Append(key string, values string) {
	key = strings.ToLower(key)
	if len(values) == 0 {
		// Find and remove the key-value pair
		for i := 0; i < len(m); i += 2 {
			if strings.ToLower(m[i]) == key {
				return
			}
		}
		return
	}
	// Key not found, append the new key-value pair
	m = append(m, key, values)
}

// Clone returns a copy of MD or nil if it's nil.
// It's copied from golang's `http.Header.Clone` implementation:
// https://cs.opensource.google/go/go/+/refs/tags/go1.23.4:src/net/http/header.go;l=94
func (m MD) Clone() MD {
	if m == nil {
		return nil
	}

	m2 := make(MD, len(m))
	copy(m2, m)

	return m2
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

	if list, ok := metadata.Get(name); ok {
		return list, true
	}

	return "", false
}

// WithMetadata attaches metadata map to a context.Context
func WithMetadata(ctx context.Context, md MD) context.Context {
	return context.WithValue(ctx, metadataKey{}, md)
}
