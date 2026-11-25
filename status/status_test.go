package status

import (
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

// TestPredefinedStatusesMirrorTheCodes covers the table: every exported status
// carries its own upstream code and a non-empty message.
func TestPredefinedStatusesMirrorTheCodes(t *testing.T) {
	cases := []struct {
		name string
		got  *grpcstatus.Status
		want codes.Code
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
	}
	for _, c := range cases {
		if got := c.got.Code(); got != c.want {
			t.Errorf("%s.Code() = %v, want %v", c.name, got, c.want)
		}
		if c.got.Message() == "" {
			t.Errorf("%s carries an empty message", c.name)
		}
	}
}

// TestErrorBuildsAnErrorWithTheCode covers the constructor and the three
// readers.
func TestErrorBuildsAnErrorWithTheCode(t *testing.T) {
	err := Error(codes.NotFound, "no such pod")
	if got := Code(err); got != codes.NotFound {
		t.Errorf("Code = %v, want NotFound", got)
	}
	if got := Convert(err).Message(); got != "no such pod" {
		t.Errorf("Convert.Message = %q, want no such pod", got)
	}
	if err.Error() != "rpc error: code = NotFound desc = no such pod" {
		t.Errorf("Error() = %q, want the upstream text", err.Error())
	}
}

// TestFromErrorReportsWhetherAnErrorCarriesAStatus covers both answers.
func TestFromErrorReportsWhetherAnErrorCarriesAStatus(t *testing.T) {
	err := Error(codes.Internal, "broken")
	s, ok := FromError(err)
	if !ok || s == nil || s.Code() != codes.Internal {
		t.Errorf("FromError(status) = (%v, %v), want (Internal, true)", s, ok)
	}

	plain := errors.New("plain failure")
	s, ok = FromError(plain)
	if ok {
		t.Error("FromError of a plain error claimed it carries a status")
	}
	if s.Code() != codes.Unknown {
		t.Errorf("FromError(plain).Code = %v, want Unknown", s.Code())
	}
	if s.Message() != "plain failure" {
		t.Errorf("FromError(plain).Message = %q, want the error text", s.Message())
	}
}

// TestNilErrorIsOK covers the nil guard every reader has.
func TestNilErrorIsOK(t *testing.T) {
	if got := Code(nil); got != codes.OK {
		t.Errorf("Code(nil) = %v, want OK", got)
	}
	if got := Convert(nil).Code(); got != codes.OK {
		t.Errorf("Convert(nil).Code = %v, want OK", got)
	}
	if _, ok := FromError(nil); !ok {
		t.Error("FromError(nil) does not report a status")
	}
}

// TestConvertReportsAPlainErrorAsUnknown covers a wrapped ordinary error.
func TestConvertReportsAPlainErrorAsUnknown(t *testing.T) {
	err := errors.New("boom")
	if got := Convert(err).Code(); got != codes.Unknown {
		t.Errorf("Convert(plain).Code = %v, want Unknown", got)
	}
	if got := Code(err); got != codes.Unknown {
		t.Errorf("Code(plain) = %v, want Unknown", got)
	}
}
