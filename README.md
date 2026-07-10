# grpcx

gRPC 的高性能替代方案，基于 ttrpc 传输，兼容 gRPC API。

## 特点
- 比标准 gRPC 快 2.5-3 倍
- 兼容 `grpc.ClientConnInterface` 和 `grpc.ServiceDesc`
- 内置 OpenTelemetry 链路追踪
- 面向 K8s 微服务设计

## 安装

```bash
go get github.com/vimcoders/grpcx
```

## 快速开始
```go
// 服务端
server := grpcx.NewServer()
server.RegisterService(&api.EchoService_ServiceDesc, &EchoHandler{})
server.ListenAndServe(context.Background(), ":50051")

// 客户端
conn, _ := grpcx.Dial("localhost:50051")
client := api.NewEchoServiceClient(conn)
resp, _ := client.Echo(context.Background(), &api.EchoRequest{Message: "hello"})
```

## 与标准 gRPC 的区别
[依赖 K8s Service 做负载均衡]

## 适用场景
[同主机/局域网、K8s 内网通信]

## 不适用场景
[跨公网、需要丰富负载均衡策略]
