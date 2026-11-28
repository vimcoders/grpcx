package metadata

import (
	"context"
	"strings"
	"testing"
)

// TestPairsNormalisesKeys covers the constructor: keys are lowercased and
// paired with their values.
func TestPairsNormalisesKeys(t *testing.T) {
	md := Pairs("Trace-Id", "abc", "K", "v")
	if got, ok := md.Get("TRACE-ID"); !ok || got != "abc" {
		t.Errorf("Get(TRACE-ID) = (%q, %v), want (abc, true)", got, ok)
	}
	if got, ok := md.Get("k"); !ok || got != "v" {
		t.Errorf("Get(k) = (%q, %v), want (v, true)", got, ok)
	}
	if len(md) != 2 {
		t.Errorf("Pairs built %d entries, want 2", len(md))
	}
}

// TestPairsPanicsOnAnOddCount covers the documented misuse.
func TestPairsPanicsOnAnOddCount(t *testing.T) {
	odd := strings.Split("k", ",")
	defer func() {
		if recover() == nil {
			t.Error("Pairs with an odd count did not panic")
		}
	}()
	Pairs(odd...)
}

// TestParsePairsBuildsMetadata covers the non-panicking constructor used at
// input boundaries: valid pairs produce the same normalized map as Pairs.
func TestParsePairsBuildsMetadata(t *testing.T) {
	md, err := ParsePairs("Trace-Id", "abc", "K", "v")
	if err != nil {
		t.Fatalf("ParsePairs: %v", err)
	}
	if got, ok := md.Get("trace-id"); !ok || got != "abc" {
		t.Errorf("Get(trace-id) = (%q, %v), want (abc, true)", got, ok)
	}
	if got, ok := md.Get("k"); !ok || got != "v" {
		t.Errorf("Get(k) = (%q, %v), want (v, true)", got, ok)
	}
}

// TestParsePairsRejectsAnOddCount covers malformed input: the caller receives
// an error and no partial map, and nothing panics.
func TestParsePairsRejectsAnOddCount(t *testing.T) {
	md, err := ParsePairs("key")
	if err == nil {
		t.Fatal("ParsePairs with an odd count returned no error")
	}
	if md != nil {
		t.Errorf("ParsePairs returned %v with the error, want nil", md)
	}
}

// TestGetMissesAKey covers the absent case.
func TestGetMissesAKey(t *testing.T) {
	if got, ok := Pairs("k", "v").Get("missing"); ok || got != "" {
		t.Errorf("Get(missing) = (%q, %v), want (\"\", false)", got, ok)
	}
}

// TestCloneReturnsAnIndependentCopy covers the copy semantics: writes to the
// clone never reach the original.
func TestCloneReturnsAnIndependentCopy(t *testing.T) {
	original := Pairs("k", "v")
	clone := original.Clone()
	clone["k"] = "changed"
	if got, _ := original.Get("k"); got != "v" {
		t.Errorf("original k = %q, want v", got)
	}
}

// TestCloneOfNilIsNil covers the nil case.
func TestCloneOfNilIsNil(t *testing.T) {
	var md MD
	if md.Clone() != nil {
		t.Error("Clone of a nil map returned a map")
	}
}

// TestContextCarriesMetadata covers WithMetadata and the two readers.
func TestContextCarriesMetadata(t *testing.T) {
	ctx := WithMetadata(context.Background(), Pairs("k", "v"))
	if md, ok := GetMetadata(ctx); !ok || md == nil {
		t.Fatalf("GetMetadata = (%v, %v), want the attached map", md, ok)
	}
	if got, ok := GetMetadataValue(ctx, "K"); !ok || got != "v" {
		t.Errorf("GetMetadataValue(K) = (%q, %v), want (v, true)", got, ok)
	}
}

// TestGetMetadataMissesAnEmptyContext covers a context without metadata.
func TestGetMetadataMissesAnEmptyContext(t *testing.T) {
	if md, ok := GetMetadata(context.Background()); ok || md != nil {
		t.Errorf("GetMetadata = (%v, %v), want (nil, false)", md, ok)
	}
	if got, ok := GetMetadataValue(context.Background(), "k"); ok || got != "" {
		t.Errorf("GetMetadataValue = (%q, %v), want (\"\", false)", got, ok)
	}
}

// TestAppendToContextBuildsMetadataFromNothing covers the first append.
func TestAppendToContextBuildsMetadataFromNothing(t *testing.T) {
	ctx := AppendToContext(context.Background(), "Trace-Id", "abc")
	if got, ok := GetMetadataValue(ctx, "trace-id"); !ok || got != "abc" {
		t.Errorf("GetMetadataValue(trace-id) = (%q, %v), want (abc, true)", got, ok)
	}
}

// TestAppendToContextMergesWithoutChangingTheParent covers the clone: the parent
// context keeps its own metadata.
func TestAppendToContextMergesWithoutChangingTheParent(t *testing.T) {
	parent := WithMetadata(context.Background(), Pairs("k", "v"))
	child := AppendToContext(parent, "extra", "1")

	if got, _ := GetMetadataValue(child, "k"); got != "v" {
		t.Errorf("child k = %q, want inherited v", got)
	}
	if got, _ := GetMetadataValue(child, "extra"); got != "1" {
		t.Errorf("child extra = %q, want 1", got)
	}
	if _, ok := GetMetadataValue(parent, "extra"); ok {
		t.Error("the parent context gained the child's metadata")
	}
}

// TestAppendToContextTreatsNilAsEmpty covers a nil map carried in the context.
func TestAppendToContextTreatsNilAsEmpty(t *testing.T) {
	parent := WithMetadata(context.Background(), nil)
	child := AppendToContext(parent, "k", "v")
	if got, ok := GetMetadataValue(child, "k"); !ok || got != "v" {
		t.Errorf("GetMetadataValue(k) = (%q, %v), want (v, true)", got, ok)
	}
}

// TestAppendToContextPanicsOnAnOddCount covers the documented misuse.
func TestAppendToContextPanicsOnAnOddCount(t *testing.T) {
	odd := strings.Split("k", ",")
	defer func() {
		if recover() == nil {
			t.Error("AppendToContext with an odd count did not panic")
		}
	}()
	AppendToContext(context.Background(), odd...)
}
