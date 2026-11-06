package transport

import (
	"context"
	"io"
	"math"
	"time"

	"github.com/vimcoders/grpcx/generated/api"

	"google.golang.org/grpc"
)

// RoundTripper is an interface representing the ability to execute a
// single HTTP transaction, obtaining the [Response] for a given [Request].
//
// A RoundTripper must be safe for concurrent use by multiple
// goroutines.
type RoundTripper interface {
	// RoundTrip executes a single HTTP transaction, returning
	// a Response for the provided Request.
	//
	// RoundTrip should not attempt to interpret the response. In
	// particular, RoundTrip must return err == nil if it obtained
	// a response, regardless of the response's HTTP status code.
	// A non-nil err should be reserved for failure to obtain a
	// response. Similarly, RoundTrip should not attempt to
	// handle higher-level protocol details such as redirects,
	// authentication, or cookies.
	//
	// RoundTrip should not modify the request, except for
	// consuming and closing the Request's Body. RoundTrip may
	// read fields of the request in a separate goroutine. Callers
	// should not mutate or reuse the request until the Response's
	// Body has been closed.
	//
	// RoundTrip must always close the body, including on errors,
	// but depending on the implementation may do so in a separate
	// goroutine even after RoundTrip returns. This means that
	// callers wanting to reuse the body for subsequent requests
	// must arrange to wait for the Close call before doing so.
	//
	// The Request's URL and Header fields must be initialized.
	grpc.ClientConnInterface
	RoundTrip(ctx context.Context, req *api.Request) (*api.Response, error)
	Ping(ctx context.Context) error
	io.Closer
}

const (
	messageHeaderLength = 8
	// 4MB 与标准 gRPC 默认值对齐
	messageLengthMax = 4 * 1024 * 1024
	// Go 1.27.3 采用的时间轮算法最小时间是1毫秒
	defaultFlushInterval = time.Millisecond
	// bw
	defaultBufSize = 1024
	// linux内核tcp发送缓冲区的默认值是16k
	defaultMaxBufSize = 16 * defaultBufSize
	// DefaultMaxStreams is the default maximum number of concurrent streams per connection.
	defaultMaxStreams = math.MaxUint16
	// DefaultTimeout is the default timeout for each request.
	defaultTimeout = 3 * time.Second
)
