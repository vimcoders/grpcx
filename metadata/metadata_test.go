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
	"testing"
)

// wantPanic runs fn and fails the test unless fn panics.
func wantPanic(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatalf("%s: expected panic, got none", name)
		}
	}()
	fn()
}

// TestPairsStoresLowercaseKeys pins the documented normalization: a key is
// stored lowercased, so the raw map lookup agrees with MD.Get.
func TestPairsStoresLowercaseKeys(t *testing.T) {
	md := Pairs("Content-Type", "application/proto")

	if _, ok := md["Content-Type"]; ok {
		t.Fatalf("Pairs stored the key with its original case")
	}
	if v, ok := md["content-type"]; !ok || v != "application/proto" {
		t.Fatalf("md[content-type] = %q, %v; want application/proto, true", v, ok)
	}
	if v, ok := md.Get("Content-Type"); !ok || v != "application/proto" {
		t.Fatalf("Get(Content-Type) = %q, %v; want application/proto, true", v, ok)
	}
}

// TestPairsMixedCaseKeysCollapse checks that keys differing only in case land
// on the same entry, last value winning.
func TestPairsMixedCaseKeysCollapse(t *testing.T) {
	md := Pairs("X-Trace", "first", "x-TRACE", "second", "x-trace", "third")

	if len(md) != 1 {
		t.Fatalf("len(md) = %d, want 1: %v", len(md), md)
	}
	if v, _ := md.Get("x-trace"); v != "third" {
		t.Fatalf("Get(x-trace) = %q, want third", v)
	}
}

func TestPairsPanicsOnOddInput(t *testing.T) {
	wantPanic(t, "Pairs", func() { Pairs("only-key") })
}

func TestPairsEmptyReturnsNonNil(t *testing.T) {
	md := Pairs()
	if md == nil {
		t.Fatalf("Pairs() = nil, want an empty non-nil MD")
	}
	if len(md) != 0 {
		t.Fatalf("len(Pairs()) = %d, want 0", len(md))
	}
}

func TestGetNormalizesQuery(t *testing.T) {
	md := Pairs("Key", "value")

	for _, key := range []string{"Key", "KEY", "key", "kEy"} {
		if v, ok := md.Get(key); !ok || v != "value" {
			t.Fatalf("Get(%q) = %q, %v; want value, true", key, v, ok)
		}
	}
	if v, ok := md.Get("missing"); ok || v != "" {
		t.Fatalf("Get(missing) = %q, %v; want \"\", false", v, ok)
	}
}

func TestAppendToContextNormalizesKeys(t *testing.T) {
	ctx := WithMetadata(context.Background(), Pairs("First", "1"))
	ctx = AppendToContext(ctx, "Second", "2")

	md, ok := GetMetadata(ctx)
	if !ok {
		t.Fatalf("GetMetadata returned no metadata")
	}
	if _, exists := md["Second"]; exists {
		t.Fatalf("AppendToContext stored the key with its original case")
	}
	if v, ok := GetMetadataValue(ctx, "SECOND"); !ok || v != "2" {
		t.Fatalf("GetMetadataValue(SECOND) = %q, %v; want 2, true", v, ok)
	}
	if v, ok := GetMetadataValue(ctx, "first"); !ok || v != "1" {
		t.Fatalf("GetMetadataValue(first) = %q, %v; want 1, true", v, ok)
	}
}

// TestAppendToContextOnEmptyContext covers the path that builds the map from
// scratch, where normalization happens inside Pairs.
func TestAppendToContextOnEmptyContext(t *testing.T) {
	ctx := AppendToContext(context.Background(), "Mixed-Case", "v")

	if v, ok := GetMetadataValue(ctx, "mixed-case"); !ok || v != "v" {
		t.Fatalf("GetMetadataValue(mixed-case) = %q, %v; want v, true", v, ok)
	}
}

func TestAppendToContextPanicsOnOddInput(t *testing.T) {
	wantPanic(t, "AppendToContext", func() {
		AppendToContext(context.Background(), "dangling")
	})
}

func TestGetMetadataValueOnBareContext(t *testing.T) {
	if v, ok := GetMetadataValue(context.Background(), "any"); ok || v != "" {
		t.Fatalf("GetMetadataValue on a bare context = %q, %v; want \"\", false", v, ok)
	}
}

func TestWithMetadataRoundTrip(t *testing.T) {
	if _, ok := GetMetadata(context.Background()); ok {
		t.Fatalf("GetMetadata on a bare context returned metadata")
	}
	md := Pairs("a", "1")
	got, ok := GetMetadata(WithMetadata(context.Background(), md))
	if !ok {
		t.Fatalf("metadata did not survive the context round trip")
	}
	if v := mustGet(t, got, "a"); v != "1" {
		t.Fatalf("Get(a) = %q, want 1", v)
	}
}

// mustGet fails the test when the key is absent.
func mustGet(t *testing.T, md MD, key string) string {
	t.Helper()
	v, ok := md.Get(key)
	if !ok {
		t.Fatalf("Get(%q): missing", key)
	}
	return v
}

func TestCloneIsIndependent(t *testing.T) {
	if got := MD(nil).Clone(); got != nil {
		t.Fatalf("nil.Clone() = %v, want nil", got)
	}

	md := Pairs("k", "v")
	clone := md.Clone()
	if v := mustGet(t, clone, "k"); v != "v" {
		t.Fatalf("clone lost the entry")
	}
	clone["k"] = "changed"
	if v := mustGet(t, md, "k"); v != "v" {
		t.Fatalf("mutating the clone changed the original")
	}
}

// TestAppendToContextDoesNotMutateParent pins the copy-on-append contract: a
// context keeps the metadata it was created with, whatever is appended to a
// context derived from it.
func TestAppendToContextDoesNotMutateParent(t *testing.T) {
	parent := WithMetadata(context.Background(), Pairs("a", "1"))
	child := AppendToContext(parent, "B", "2")

	parentMD, ok := GetMetadata(parent)
	if !ok {
		t.Fatalf("parent lost its metadata")
	}
	if _, exists := parentMD["b"]; exists {
		t.Fatalf("AppendToContext wrote into the parent map: %v", parentMD)
	}
	if v := mustGet(t, parentMD, "a"); v != "1" {
		t.Fatalf("parent a = %q, want 1", v)
	}

	childMD, ok := GetMetadata(child)
	if !ok {
		t.Fatalf("child has no metadata")
	}
	if v := mustGet(t, childMD, "b"); v != "2" {
		t.Fatalf("child b = %q, want 2", v)
	}
	if v := mustGet(t, childMD, "a"); v != "1" {
		t.Fatalf("child lost the inherited entry")
	}
}

// TestAppendToContextOnNilMetadata checks that a nil MD carried in the context
// is treated as empty instead of panicking on a write to a nil map.
func TestAppendToContextOnNilMetadata(t *testing.T) {
	ctx := AppendToContext(WithMetadata(context.Background(), MD(nil)), "K", "v")

	md, ok := GetMetadata(ctx)
	if !ok || md == nil {
		t.Fatalf("GetMetadata after append on nil MD = %v, %v", md, ok)
	}
	if v := mustGet(t, md, "k"); v != "v" {
		t.Fatalf("k = %q, want v", v)
	}
}

// TestAppendToContextEmptyKeepsEntries covers appending nothing: the derived
// context still carries the inherited entries.
func TestAppendToContextEmptyKeepsEntries(t *testing.T) {
	ctx := AppendToContext(WithMetadata(context.Background(), Pairs("a", "1")))

	if v, ok := GetMetadataValue(ctx, "a"); !ok || v != "1" {
		t.Fatalf("GetMetadataValue(a) = %q, %v; want 1, true", v, ok)
	}
}
