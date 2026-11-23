package status

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

// TestPackageStatuses pins every exported status: the code it carries, the
// message, and what its Err method yields. The balancer builds its errors from
// these values, so a wrong code here would surface as a wrong wire status.
func TestPackageStatuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    *grpcstatus.Status
		code codes.Code
	}{
		{"OK", OK, codes.OK},
		{"Canceled", Canceled, codes.Canceled},
		{"Unknown", Unknown, codes.Unknown},
		{"InvalidArgument", InvalidArgument, codes.InvalidArgument},
		{"DeadlineExceeded", DeadlineExceeded, codes.DeadlineExceeded},
		{"NotFound", NotFound, codes.NotFound},
		{"AlreadyExists", AlreadyExists, codes.AlreadyExists},
		{"PermissionDenied", PermissionDenied, codes.PermissionDenied},
		{"ResourceExhausted", ResourceExhausted, codes.ResourceExhausted},
		{"FailedPrecondition", FailedPrecondition, codes.FailedPrecondition},
		{"Aborted", Aborted, codes.Aborted},
		{"OutOfRange", OutOfRange, codes.OutOfRange},
		{"Unimplemented", Unimplemented, codes.Unimplemented},
		{"Internal", Internal, codes.Internal},
		{"Unavailable", Unavailable, codes.Unavailable},
		{"DataLoss", DataLoss, codes.DataLoss},
		{"Unauthenticated", Unauthenticated, codes.Unauthenticated},
	} {
		if tc.s == nil {
			t.Fatalf("%s = nil", tc.name)
		}
		if got := tc.s.Code(); got != tc.code {
			t.Fatalf("%s.Code() = %v, want %v", tc.name, got, tc.code)
		}
		if got := tc.s.Message(); got != tc.code.String() {
			t.Fatalf("%s.Message() = %q, want %q", tc.name, got, tc.code.String())
		}
		err := tc.s.Err()
		if tc.code == codes.OK {
			if err != nil {
				t.Fatalf("OK.Err() = %v, want nil", err)
			}
			continue
		}
		if err == nil {
			t.Fatalf("%s.Err() = nil, want an error", tc.name)
		}
		if got := Code(err); got != tc.code {
			t.Fatalf("Code(%s.Err()) = %v, want %v", tc.name, got, tc.code)
		}
	}
}

// TestFromError covers the three shapes callers hand over: a status error, a
// wrapped one and a plain error.
func TestFromError(t *testing.T) {
	src := Error(codes.NotFound, "missing")
	s, ok := FromError(src)
	if !ok || s == nil {
		t.Fatalf("FromError(status error) = %v, %v", s, ok)
	}
	if s.Code() != codes.NotFound || s.Message() != "missing" {
		t.Fatalf("FromError(status error) = code %v, message %q", s.Code(), s.Message())
	}

	wrapped := fmt.Errorf("call failed: %w", src)
	s, ok = FromError(wrapped)
	if !ok || s.Code() != codes.NotFound {
		t.Fatalf("FromError(wrapped) = code %v, ok %v", s.Code(), ok)
	}

	s, ok = FromError(errors.New("plain"))
	if ok {
		t.Fatalf("FromError(plain) reported a status: %v", s)
	}
	if s == nil || s.Code() != codes.Unknown {
		t.Fatalf("FromError(plain) = %v, want code Unknown", s)
	}

	// grpc reports a nil error as an absent status: ok is true but the status
	// pointer is nil, and a nil *Status answers OK.
	s, ok = FromError(nil)
	if !ok {
		t.Fatalf("FromError(nil) reported ok = false")
	}
	if s != nil {
		t.Fatalf("FromError(nil) = %v, want nil", s)
	}
	if got := s.Code(); got != codes.OK {
		t.Fatalf("nil status Code() = %v, want OK", got)
	}
}

func TestConvert(t *testing.T) {
	src := Error(codes.ResourceExhausted, "quota")
	if got := Convert(src); got.Code() != codes.ResourceExhausted || got.Message() != "quota" {
		t.Fatalf("Convert(status error) = %v / %q", got.Code(), got.Message())
	}

	plain := errors.New("plain failure")
	if got := Convert(plain); got.Code() != codes.Unknown || got.Message() != "plain failure" {
		t.Fatalf("Convert(plain) = %v / %q, want Unknown / plain failure", got.Code(), got.Message())
	}

	// A wrapped error keeps the inner code but carries the wrapper's text.
	wrapped := fmt.Errorf("outer: %w", src)
	s := Convert(wrapped)
	if s.Code() != codes.ResourceExhausted {
		t.Fatalf("Convert(wrapped).Code() = %v, want ResourceExhausted", s.Code())
	}
	if !strings.Contains(s.Message(), "quota") {
		t.Fatalf("Convert(wrapped).Message() = %q, want it to mention quota", s.Message())
	}

	if got := Convert(nil); got.Code() != codes.OK || got.Message() != "" {
		t.Fatalf("Convert(nil) = %v / %q, want OK / empty", got.Code(), got.Message())
	}
}

func TestCode(t *testing.T) {
	if got := Code(nil); got != codes.OK {
		t.Fatalf("Code(nil) = %v, want OK", got)
	}
	if got := Code(Error(codes.DeadlineExceeded, "late")); got != codes.DeadlineExceeded {
		t.Fatalf("Code(status error) = %v, want DeadlineExceeded", got)
	}
	if got := Code(errors.New("plain")); got != codes.Unknown {
		t.Fatalf("Code(plain) = %v, want Unknown", got)
	}
}

// TestError checks the builder: the code and message survive, and the OK /
// empty combination collapses to nil so callers can return it as success.
func TestError(t *testing.T) {
	err := Error(codes.InvalidArgument, "bad field")
	if err == nil {
		t.Fatalf("Error returned nil")
	}
	s, ok := FromError(err)
	if !ok || s.Code() != codes.InvalidArgument || s.Message() != "bad field" {
		t.Fatalf("Error round trip = ok %v, code %v, message %q", ok, s.Code(), s.Message())
	}

	if err := Error(codes.OK, ""); err != nil {
		t.Fatalf("Error(OK, \"\") = %v, want nil", err)
	}

	// Any OK code collapses to nil, message or not: an OK status is success.
	if err := Error(codes.OK, "not really an error"); err != nil {
		t.Fatalf("Error(OK, msg) = %v, want nil", err)
	}
}
