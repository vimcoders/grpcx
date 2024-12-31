// Code generated by Kitex v0.11.3. DO NOT EDIT.
package chat

import (
	server "github.com/cloudwego/kitex/server"
	goapi "kitex/kitex_gen/goapi"
)

// NewServer creates a server.Server with the given handler and options.
func NewServer(handler goapi.Chat, opts ...server.Option) server.Server {
	var options []server.Option

	options = append(options, opts...)

	svr := server.NewServer(options...)
	if err := svr.RegisterService(serviceInfo(), handler); err != nil {
		panic(err)
	}
	return svr
}

func RegisterService(svr server.Server, handler goapi.Chat, opts ...server.RegisterOption) error {
	return svr.RegisterService(serviceInfo(), handler, opts...)
}