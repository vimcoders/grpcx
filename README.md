# grpcx

k8s微服务通信方案，传输层transport部分源码来自k8s容器运行时containerd中的ttrpc，兼容 gRPC API。

## 特点
- 兼容 `grpc.ClientConnInterface` 和 `grpc.ServiceDesc`
- 内置 OpenTelemetry 链路追踪
- 面向 K8s 微服务设计
- 连接池
- 负载均衡 DNS解析
- 健康检查
- 元数据传递 mdtadata.MD
- 低延迟 1kb echo在Intel i5-14600KF (Linux)并发测试场景中平均延迟降低到4.7–6.0 微秒
- 高性能 grpcx leads 1.36x~3.04x.

### 帧格式

| 4B StreamID | 4B Length | protobuf Header |
|-------------|-----------|-----------------|
| 4B StreamID | 4B Length |     Payload     |

- **发送侧**：channel.Send(streamID, h, payload) 逐段写 8B 头 + 内容，每段独立成帧。
- **接收侧**：第一次 Recv 得 header 段采用默认的解析器 Unmarshal 出 api.Header，第二次 Recv 得 payload 段采用自定义的解析器得Req。
- **Header**: 很小（几十字节），payload 可以到 4MB（messageLengthMax），两者编解码互不影响。

## Performance

Benchmarks run on Intel i5-14600KF (Linux), same-host Docker bridge network. client benchmark container runs unconstrained, server containers each limited to 1 CPU (compose cpus: "1"). -cpu=N sets GOMAXPROCS=N: N parallel workers via b.RunParallel over a single client connection. Speedup > 1.0 means grpcx faster.

### Latency & Throughput

| Payload | Concurrency | gRPC ns/op | grpcx ns/op | Speedup | gRPC QPS | grpcx QPS | gRPC allocs/op | grpcx allocs/op |
|---------|-------------|------------|-------------|---------|----------|-----------|----------------|-----------------|
| 1KB     | 64          | 14,243     | 4,822       | 2.95x   | 70,209   | 207,382   | 75             | 27              |
| 1KB     | 128         | 14,589     | 4,818       | 3.03x   | 68,544   | 207,555   | 75             | 27              |
| 1KB     | 256         | 13,748     | 5,005       | 2.75x   | 72,737   | 199,800   | 75             | 27              |
| 1KB     | 512         | 13,361     | 5,304       | 2.52x   | 74,844   | 188,536   | 75             | 27              |
| 1KB     | 1024        | 14,636     | 6,449       | 2.27x   | 68,324   | 155,062   | 74             | 27              |
| 5KB     | 64          | 19,873     | 11,988      | 1.66x   | 50,319   | 83,416    | 75             | 27              |
| 5KB     | 128         | 19,777     | 12,124      | 1.63x   | 50,563   | 82,481    | 75             | 27              |
| 5KB     | 256         | 18,151     | 12,721      | 1.43x   | 55,093   | 78,610    | 75             | 27              |
| 5KB     | 512         | 20,621     | 12,782      | 1.61x   | 48,494   | 78,235    | 74             | 27              |
| 5KB     | 1024        | 23,967     | 12,550      | 1.91x   | 41,724   | 79,681    | 74             | 27              |
| 1MB     | 64          | 1,515,922  | 931,344     | 1.63x   | 659      | 1,073     | 94             | 28              |
| 1MB     | 128         | 1,753,118  | 903,216     | 1.94x   | 570      | 1,107     | 94             | 28              |
| 1MB     | 256         | 1,623,704  | 914,809     | 1.77x   | 615      | 1,093     | 96             | 28              |
| 1MB     | 512         | 1,660,598  | 952,253     | 1.74x   | 602      | 1,050     | 98             | 28              |
| 1MB     | 1024        | 1,807,317  | 917,995     | 1.97x   | 553      | 1,089     | 96             | 28              |

### Summary

- **1KB**: grpcx leads across all concurrency levels, speedup 2.27x~3.03x, peak QPS 208K.
- **5KB**: grpcx leads 1.43x~1.91x; latency improved ~8% vs previous release.
- **1MB**: grpcx leads across all concurrency levels, 1.63x~1.97x, latency 0.90~0.95ms.
- **Memory**: 27~28 allocs/op (vs gRPC 74~98); 1KB B/op 3.7KB (vs gRPC 5.9KB).
## 架构设计：与标准 gRPC 分层对应
grpcx 采用「上层语义全兼容 + 底层传输层替换」的架构设计，完整保留 gRPC 经过验证的核心编程模型，仅替换标准化但沉重的 HTTP/2 传输栈，实现内网场景的极致性能。

| 分层 | 标准 gRPC 实现 | grpcx 实现 | 核心说明 |
|------|-------------|------------|---------|
| **业务契约层** | Proto3 标准 IDL + protoc 代码生成<br>生成 `ServiceDesc`、客户端桩与服务端骨架 | 100% 复用 gRPC 生成的 `ServiceDesc` 与业务代码<br>无需重新定义 proto、无需重新生成代码 | 业务层零差异，最大化复用现有 gRPC 生态 |
| **客户端接口层** | `grpc.ClientConnInterface` 标准接口<br>`grpc.Dial()` 丰富配置项 | 完整实现 `grpc.ClientConnInterface` 接口<br>`grpcx.Dial()` 精简核心配置 | 接口完全兼容，业务代码无需修改即可替换 |
| **服务端接口层** | `grpc.Server` + `RegisterService()` 注册服务 | `grpcx.Server` + `RegisterService()` 直接接收 `grpc.ServiceDesc` | 编程模型完全对齐，学习成本极低 |
| **核心RPC调度层** | 基于 HTTP/2 Stream 映射 RPC 调用<br>支持一元 / 客户端流 / 服务端流 / 双向流 | 基于 ttrpc 轻量流实现 RPC 调度<br>全量支持四种 RPC 模式，语义与 gRPC 完全对齐 | RPC 核心语义一致，仅底层流载体不同 |
| **扩展拦截器层** | 标准一元 / 流式拦截器接口<br>丰富的第三方拦截器生态 | 独立实现拦截器体系<br>支持一元与流式、客户端与服务端双端拦截 | 设计思想一致，无法直接复用 gRPC 第三方拦截器 |
| **服务治理层** | 可插拔 Resolver / Balancer<br>支持 etcd / Consul 等多种服务发现<br>丰富的负载均衡策略 | 内置 DNS 服务发现 + 基础负载均衡<br>内置连接池与健康检查 | gRPC 面向通用场景能力更全；grpcx 保留 K8s 内网核心能力 |
| **元数据上下文层** | `grpc/metadata` 基于 HTTP/2 Header 传递 | `grpcx/metadata` 基于协议帧内嵌传递 | 业务使用方式完全一致，仅底层承载载体不同 |
| **序列化层** | 默认 Protobuf 序列化，支持自定义 Codec | `grpcx/encoding` 模块，默认 Protobuf 序列化 | 序列化协议完全一致，复用 protoc 生成代码 |
| **传输协议层** | HTTP/2 + gRPC 帧协议<br>标准公开协议，包含完整流控、拥塞控制能力 | ttrpc 轻量化二进制协议（源自 containerd）<br>仅保留核心帧封装与流复用能力 | **最核心差异层**，也是 2.5~3 倍性能优势的来源 |
| **可观测性层** | OpenTelemetry 插件式扩展<br>链路追踪、指标、日志生态完善 | 内置 OpenTelemetry 链路追踪<br>集成在框架核心，开箱即用 | grpcx 面向云原生场景默认标配可观测能力 |
| **网络IO层** | 基于 Go 标准库 `net/http2`<br>底层 TCP 传输 | 基于 Go 原生 `net.Conn`<br>底层 TCP 传输 | 均基于 TCP 传输，grpcx 绕过 HTTP/2 栈直接操作裸连接 |

> 设计本质：grpcx 不是 gRPC 的分支或魔改版本，而是一套完整独立的 RPC 实现。它保留了 gRPC 成熟的核心语义与编程模型，只替换了为通用性而设计的重型传输层，从而在 K8s 内网场景下获得极致的性能表现。

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

## 适用场景

- K8s 内网微服务通信
- 对延迟敏感的场景
- 同主机/局域网部署

## 不适用场景

- 跨公网通信（需要 TLS、丰富重试策略）
- 需要复杂负载均衡（如一致性哈希）
- 与标准 gRPC 服务端互通（协议不同）
