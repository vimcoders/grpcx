package status

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var OK = status.New(codes.OK, codes.OK.String())                                                 // 执行成功
var Canceled = status.New(codes.Canceled, codes.Canceled.String())                               // 被取消
var Unknown = status.New(codes.Unknown, codes.Unknown.String())                                  // 未知错误
var InvalidArgument = status.New(codes.InvalidArgument, codes.InvalidArgument.String())          // 客户端传参错误
var DeadlineExceeded = status.New(codes.DeadlineExceeded, codes.DeadlineExceeded.String())       // 超时
var NotFound = status.New(codes.NotFound, codes.NotFound.String())                               // 没有找到对应的方法
var AlreadyExists = status.New(codes.AlreadyExists, codes.AlreadyExists.String())                // 已经存在
var PermissionDenied = status.New(codes.PermissionDenied, codes.PermissionDenied.String())       // 权限不足
var ResourceExhausted = status.New(codes.ResourceExhausted, codes.ResourceExhausted.String())    // 告诉客户端退避
var FailedPrecondition = status.New(codes.FailedPrecondition, codes.FailedPrecondition.String()) // 不满足前置条件
var Aborted = status.New(codes.Aborted, codes.Aborted.String())                                  // 并发冲突
var OutOfRange = status.New(codes.OutOfRange, codes.OutOfRange.String())                         // 超出范围
var Unimplemented = status.New(codes.Unimplemented, codes.Unimplemented.String())                // 未实现
var Internal = status.New(codes.Internal, codes.Internal.String())                               // 服务器内部错误
var Unavailable = status.New(codes.Unavailable, codes.Unavailable.String())                      // 框架默认，可重试
var DataLoss = status.New(codes.DataLoss, codes.DataLoss.String())                               // 数据丢失
var Unauthenticated = status.New(codes.Unauthenticated, codes.Unauthenticated.String())          // 需要重新登录 / 换 Token

func FromError(err error) (s *status.Status, ok bool) {
	return status.FromError(err)
}

func Convert(err error) *status.Status {
	return status.Convert(err)
}

func Code(err error) codes.Code {
	return status.Code(err)
}
