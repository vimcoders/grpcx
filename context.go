package ttrpc

const (
	grpc_authority = "Authority"
)

type contextKey string

var (
	Authority contextKey = grpc_authority
)
