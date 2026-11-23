package status

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The predefined statuses mirror google.golang.org/grpc/codes one to one, so
// callers can use them exactly as they would the upstream package.
var (
	// OK is not an error; returned on success.
	OK = status.New(codes.OK, codes.OK.String())
	// Canceled means the operation was canceled, typically by the caller.
	Canceled = status.New(codes.Canceled, codes.Canceled.String())
	// Unknown is an unknown error, or a code this client does not recognize.
	Unknown = status.New(codes.Unknown, codes.Unknown.String())
	// InvalidArgument means an argument is problematic regardless of system state.
	InvalidArgument = status.New(codes.InvalidArgument, codes.InvalidArgument.String())
	// DeadlineExceeded means the deadline expired before the operation completed.
	DeadlineExceeded = status.New(codes.DeadlineExceeded, codes.DeadlineExceeded.String())
	// NotFound means the requested entity does not exist.
	NotFound = status.New(codes.NotFound, codes.NotFound.String())
	// AlreadyExists means the entity we tried to create already exists.
	AlreadyExists = status.New(codes.AlreadyExists, codes.AlreadyExists.String())
	// PermissionDenied means the caller lacks permission for this operation.
	PermissionDenied = status.New(codes.PermissionDenied, codes.PermissionDenied.String())
	// ResourceExhausted means a resource such as a quota or disk is exhausted.
	ResourceExhausted = status.New(codes.ResourceExhausted, codes.ResourceExhausted.String())
	// FailedPrecondition means the system is not in the state the operation requires.
	FailedPrecondition = status.New(codes.FailedPrecondition, codes.FailedPrecondition.String())
	// Aborted means the operation was aborted by a concurrency conflict.
	Aborted = status.New(codes.Aborted, codes.Aborted.String())
	// OutOfRange means the operation was attempted past the valid range.
	OutOfRange = status.New(codes.OutOfRange, codes.OutOfRange.String())
	// Unimplemented means the operation is not implemented or not enabled in this service.
	Unimplemented = status.New(codes.Unimplemented, codes.Unimplemented.String())
	// Internal means an invariant expected by the system was broken.
	Internal = status.New(codes.Internal, codes.Internal.String())
	// Unavailable means the service is unavailable, most likely transient.
	Unavailable = status.New(codes.Unavailable, codes.Unavailable.String())
	// DataLoss means unrecoverable data loss or corruption.
	DataLoss = status.New(codes.DataLoss, codes.DataLoss.String())
	// Unauthenticated means authentication credentials are missing or invalid.
	Unauthenticated = status.New(codes.Unauthenticated, codes.Unauthenticated.String())
)

// FromError converts err into a *status.Status. ok is false when err does not
// carry one.
func FromError(err error) (s *status.Status, ok bool) {
	return status.FromError(err)
}

// Convert returns the *status.Status carried by err, or a status built from
// codes.Unknown when err carries none.
func Convert(err error) *status.Status {
	return status.Convert(err)
}

// Code returns the codes.Code of err, or codes.Unknown when err carries none.
func Code(err error) codes.Code {
	return status.Code(err)
}

// Error builds an error carrying code c and message msg.
func Error(c codes.Code, msg string) error {
	return status.Error(c, msg)
}
