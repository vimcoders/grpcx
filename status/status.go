package status

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var OK = status.New(codes.OK, codes.OK.String())                                                 // not an error; returned on success
var Canceled = status.New(codes.Canceled, codes.Canceled.String())                               // canceled, typically by the caller
var Unknown = status.New(codes.Unknown, codes.Unknown.String())                                  // unknown error, or a code this client does not recognize
var InvalidArgument = status.New(codes.InvalidArgument, codes.InvalidArgument.String())          // argument is problematic regardless of system state
var DeadlineExceeded = status.New(codes.DeadlineExceeded, codes.DeadlineExceeded.String())       // deadline expired before the operation completed
var NotFound = status.New(codes.NotFound, codes.NotFound.String())                               // the requested entity does not exist
var AlreadyExists = status.New(codes.AlreadyExists, codes.AlreadyExists.String())                // the entity we tried to create already exists
var PermissionDenied = status.New(codes.PermissionDenied, codes.PermissionDenied.String())       // the caller lacks permission for this operation
var ResourceExhausted = status.New(codes.ResourceExhausted, codes.ResourceExhausted.String())    // a resource such as a quota or disk is exhausted
var FailedPrecondition = status.New(codes.FailedPrecondition, codes.FailedPrecondition.String()) // the system is not in the state the operation requires
var Aborted = status.New(codes.Aborted, codes.Aborted.String())                                  // aborted by a concurrency conflict
var OutOfRange = status.New(codes.OutOfRange, codes.OutOfRange.String())                         // the operation was attempted past the valid range
var Unimplemented = status.New(codes.Unimplemented, codes.Unimplemented.String())                // not implemented or not enabled in this service
var Internal = status.New(codes.Internal, codes.Internal.String())                               // an invariant expected by the system was broken
var Unavailable = status.New(codes.Unavailable, codes.Unavailable.String())                      // the service is unavailable, most likely transient
var DataLoss = status.New(codes.DataLoss, codes.DataLoss.String())                               // unrecoverable data loss or corruption
var Unauthenticated = status.New(codes.Unauthenticated, codes.Unauthenticated.String())          // missing or invalid authentication credentials

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
