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

| 特性 | grpcx | 标准 gRPC |
|------|-------|-----------|
| 传输层 | ttrpc | HTTP/2 |
| 连接模型 | 连接池 | 连接池 |
| 负载均衡 | K8s Service | 客户端 LB |
| 性能 | 快 2.5-3 倍 | 基准 |

## 适用场景

- K8s 内网微服务通信
- 对延迟敏感的场景
- 同主机/局域网部署

## 不适用场景

- 跨公网通信（需要 TLS、丰富重试策略）
- 需要复杂负载均衡（如一致性哈希）
- 与标准 gRPC 服务端互通（协议不同）
