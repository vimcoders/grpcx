// Code generated by Kitex v0.11.3. DO NOT EDIT.

package chat

import (
	"context"
	"errors"
	client "github.com/cloudwego/kitex/client"
	kitex "github.com/cloudwego/kitex/pkg/serviceinfo"
	streaming "github.com/cloudwego/kitex/pkg/streaming"
	proto "google.golang.org/protobuf/proto"
	goapi "kitex/kitex_gen/goapi"
)

var errInvalidMessageType = errors.New("invalid message type for service method handler")

var serviceMethods = map[string]kitex.MethodInfo{
	"Chat": kitex.NewMethodInfo(
		chatHandler,
		newChatArgs,
		newChatResult,
		false,
		kitex.WithStreamingMode(kitex.StreamingUnary),
	),
}

var (
	chatServiceInfo                = NewServiceInfo()
	chatServiceInfoForClient       = NewServiceInfoForClient()
	chatServiceInfoForStreamClient = NewServiceInfoForStreamClient()
)

// for server
func serviceInfo() *kitex.ServiceInfo {
	return chatServiceInfo
}

// for stream client
func serviceInfoForStreamClient() *kitex.ServiceInfo {
	return chatServiceInfoForStreamClient
}

// for client
func serviceInfoForClient() *kitex.ServiceInfo {
	return chatServiceInfoForClient
}

// NewServiceInfo creates a new ServiceInfo containing all methods
func NewServiceInfo() *kitex.ServiceInfo {
	return newServiceInfo(false, true, true)
}

// NewServiceInfo creates a new ServiceInfo containing non-streaming methods
func NewServiceInfoForClient() *kitex.ServiceInfo {
	return newServiceInfo(false, false, true)
}
func NewServiceInfoForStreamClient() *kitex.ServiceInfo {
	return newServiceInfo(true, true, false)
}

func newServiceInfo(hasStreaming bool, keepStreamingMethods bool, keepNonStreamingMethods bool) *kitex.ServiceInfo {
	serviceName := "Chat"
	handlerType := (*goapi.Chat)(nil)
	methods := map[string]kitex.MethodInfo{}
	for name, m := range serviceMethods {
		if m.IsStreaming() && !keepStreamingMethods {
			continue
		}
		if !m.IsStreaming() && !keepNonStreamingMethods {
			continue
		}
		methods[name] = m
	}
	extra := map[string]interface{}{
		"PackageName": "pb",
	}
	if hasStreaming {
		extra["streaming"] = hasStreaming
	}
	svcInfo := &kitex.ServiceInfo{
		ServiceName:     serviceName,
		HandlerType:     handlerType,
		Methods:         methods,
		PayloadCodec:    kitex.Protobuf,
		KiteXGenVersion: "v0.11.3",
		Extra:           extra,
	}
	return svcInfo
}

func chatHandler(ctx context.Context, handler interface{}, arg, result interface{}) error {
	switch s := arg.(type) {
	case *streaming.Args:
		st := s.Stream
		req := new(goapi.ChatRequest)
		if err := st.RecvMsg(req); err != nil {
			return err
		}
		resp, err := handler.(goapi.Chat).Chat(ctx, req)
		if err != nil {
			return err
		}
		return st.SendMsg(resp)
	case *ChatArgs:
		success, err := handler.(goapi.Chat).Chat(ctx, s.Req)
		if err != nil {
			return err
		}
		realResult := result.(*ChatResult)
		realResult.Success = success
		return nil
	default:
		return errInvalidMessageType
	}
}
func newChatArgs() interface{} {
	return &ChatArgs{}
}

func newChatResult() interface{} {
	return &ChatResult{}
}

type ChatArgs struct {
	Req *goapi.ChatRequest
}

func (p *ChatArgs) FastRead(buf []byte, _type int8, number int32) (n int, err error) {
	if !p.IsSetReq() {
		p.Req = new(goapi.ChatRequest)
	}
	return p.Req.FastRead(buf, _type, number)
}

func (p *ChatArgs) FastWrite(buf []byte) (n int) {
	if !p.IsSetReq() {
		return 0
	}
	return p.Req.FastWrite(buf)
}

func (p *ChatArgs) Size() (n int) {
	if !p.IsSetReq() {
		return 0
	}
	return p.Req.Size()
}

func (p *ChatArgs) Marshal(out []byte) ([]byte, error) {
	if !p.IsSetReq() {
		return out, nil
	}
	return proto.Marshal(p.Req)
}

func (p *ChatArgs) Unmarshal(in []byte) error {
	msg := new(goapi.ChatRequest)
	if err := proto.Unmarshal(in, msg); err != nil {
		return err
	}
	p.Req = msg
	return nil
}

var ChatArgs_Req_DEFAULT *goapi.ChatRequest

func (p *ChatArgs) GetReq() *goapi.ChatRequest {
	if !p.IsSetReq() {
		return ChatArgs_Req_DEFAULT
	}
	return p.Req
}

func (p *ChatArgs) IsSetReq() bool {
	return p.Req != nil
}

func (p *ChatArgs) GetFirstArgument() interface{} {
	return p.Req
}

type ChatResult struct {
	Success *goapi.ChatResponse
}

var ChatResult_Success_DEFAULT *goapi.ChatResponse

func (p *ChatResult) FastRead(buf []byte, _type int8, number int32) (n int, err error) {
	if !p.IsSetSuccess() {
		p.Success = new(goapi.ChatResponse)
	}
	return p.Success.FastRead(buf, _type, number)
}

func (p *ChatResult) FastWrite(buf []byte) (n int) {
	if !p.IsSetSuccess() {
		return 0
	}
	return p.Success.FastWrite(buf)
}

func (p *ChatResult) Size() (n int) {
	if !p.IsSetSuccess() {
		return 0
	}
	return p.Success.Size()
}

func (p *ChatResult) Marshal(out []byte) ([]byte, error) {
	if !p.IsSetSuccess() {
		return out, nil
	}
	return proto.Marshal(p.Success)
}

func (p *ChatResult) Unmarshal(in []byte) error {
	msg := new(goapi.ChatResponse)
	if err := proto.Unmarshal(in, msg); err != nil {
		return err
	}
	p.Success = msg
	return nil
}

func (p *ChatResult) GetSuccess() *goapi.ChatResponse {
	if !p.IsSetSuccess() {
		return ChatResult_Success_DEFAULT
	}
	return p.Success
}

func (p *ChatResult) SetSuccess(x interface{}) {
	p.Success = x.(*goapi.ChatResponse)
}

func (p *ChatResult) IsSetSuccess() bool {
	return p.Success != nil
}

func (p *ChatResult) GetResult() interface{} {
	return p.Success
}

type kClient struct {
	c client.Client
}

func newServiceClient(c client.Client) *kClient {
	return &kClient{
		c: c,
	}
}

func (p *kClient) Chat(ctx context.Context, Req *goapi.ChatRequest) (r *goapi.ChatResponse, err error) {
	var _args ChatArgs
	_args.Req = Req
	var _result ChatResult
	if err = p.c.Call(ctx, "Chat", &_args, &_result); err != nil {
		return
	}
	return _result.GetSuccess(), nil
}