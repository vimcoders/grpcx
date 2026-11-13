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
- 低延迟 1KB echo 在 Intel i5-14600KF (Linux) 单连接 32–256 并行 worker 下平均延迟 7.97–8.09 微秒
- 高性能 grpcx leads 1.56x~2.64x.

### 帧格式

| 4B StreamID | 4B Length | protobuf Header |
|-------------|-----------|-----------------|
| 4B StreamID | 4B Length |     Payload     |

- **发送侧**：channel.Send(streamID, h, payload) 逐段写 8B 头 + 内容，每段独立成帧。
- **接收侧**：第一次 Recv 得 header 段采用默认的解析器 Unmarshal 出 api.Header，第二次 Recv 得 payload 段采用自定义的解析器得Req。
- **Header**: 很小（几十字节），payload 可以到 4MB（messageLengthMax），两者编解码互不影响。

## Performance

Benchmarks run on Intel i5-14600KF (Linux), same-host Docker bridge network. client benchmark container runs unconstrained, server containers each limited to 1 CPU (compose cpus: "1"). -cpu=N sets GOMAXPROCS=N: N parallel workers via b.RunParallel over a single client connection. Speedup > 1.0 means grpcx faster.

### startup
```bash
docker compose up -d --build --wait grpc grpcx
docker compose run bench-1k 
docker compose run bench-5k 
docker compose run bench-1024k 
```

### Latency & Throughput

| Payload | Concurrency | gRPC ns/op | grpcx ns/op | Speedup | gRPC QPS | grpcx QPS | gRPC allocs/op | grpcx allocs/op |
|---------|-------------|------------|-------------|---------|----------|-----------|----------------|-----------------|
| 1KB     | 1           | 84,471     | 45,207      | 1.87x   | 11,838   | 22,120    | 82             | 28              |
| 1KB     | 2           | 56,749     | 33,722      | 1.68x   | 17,621   | 29,654    | 79             | 28              |
| 1KB     | 4           | 39,165     | 20,185      | 1.94x   | 25,533   | 49,542    | 77             | 27              |
| 1KB     | 8           | 28,540     | 14,534      | 1.96x   | 35,039   | 68,804    | 76             | 27              |
| 1KB     | 16          | 22,112     | 11,249      | 1.97x   | 45,224   | 88,897    | 75             | 27              |
| 1KB     | 32          | 17,688     | 8,018       | 2.21x   | 56,536   | 124,719   | 75             | 27              |
| 1KB     | 64          | 14,880     | 8,024       | 1.85x   | 67,204   | 124,626   | 75             | 27              |
| 1KB     | 128         | 14,232     | 7,969       | 1.79x   | 70,264   | 125,486   | 75             | 27              |
| 1KB     | 256         | 14,164     | 8,091       | 1.75x   | 70,602   | 123,594   | 75             | 27              |
| 5KB     | 1           | 99,148     | 52,264      | 1.90x   | 10,086   | 19,134    | 82             | 28              |
| 5KB     | 2           | 64,296     | 35,546      | 1.81x   | 15,553   | 28,133    | 79             | 28              |
| 5KB     | 4           | 46,570     | 25,846      | 1.80x   | 21,473   | 38,691    | 77             | 28              |
| 5KB     | 8           | 35,258     | 16,357      | 2.16x   | 28,362   | 61,136    | 76             | 28              |
| 5KB     | 16          | 28,536     | 11,555      | 2.47x   | 35,043   | 86,543    | 76             | 27              |
| 5KB     | 32          | 25,327     | 12,799      | 1.98x   | 39,484   | 78,131    | 75             | 27              |
| 5KB     | 64          | 21,567     | 12,966      | 1.66x   | 46,367   | 77,125    | 75             | 27              |
| 5KB     | 128         | 20,009     | 12,864      | 1.56x   | 49,978   | 77,736    | 75             | 27              |
| 5KB     | 256         | 21,274     | 12,802      | 1.66x   | 47,006   | 78,113    | 75             | 27              |
| 1MB     | 1           | 1,555,113  | 866,601     | 1.79x   | 643      | 1,154     | 113            | 28              |
| 1MB     | 2           | 1,553,707  | 588,344     | 2.64x   | 644      | 1,700     | 111            | 28              |
| 1MB     | 4           | 1,569,641  | 599,411     | 2.62x   | 637      | 1,668     | 106            | 28              |
| 1MB     | 8           | 1,515,774  | 669,909     | 2.26x   | 660      | 1,493     | 107            | 28              |
| 1MB     | 16          | 1,545,851  | 702,529     | 2.20x   | 647      | 1,423     | 105            | 28              |
| 1MB     | 32          | 1,460,575  | 698,209     | 2.09x   | 685      | 1,432     | 97             | 28              |
| 1MB     | 64          | 1,478,256  | 690,190     | 2.14x   | 676      | 1,449     | 98             | 28              |
| 1MB     | 128         | 1,554,738  | 700,823     | 2.22x   | 643      | 1,427     | 95             | 28              |
| 1MB     | 256         | 1,487,315  | 677,932     | 2.19x   | 672      | 1,475     | 91             | 28              |

### Summary

- **1KB**: grpcx leads at every concurrency level, speedup 1.68x~2.21x, peak QPS 125,486 at 128 workers.
- **5KB**: grpcx leads at every concurrency level, speedup 1.56x~2.47x, peak QPS 86,543 at 16 workers.
- **1MB**: grpcx leads at every concurrency level, speedup 1.79x~2.64x, latency 0.59~0.87 ms.
- **Memory**: 27~28 allocs/op (vs gRPC 75~113) at every payload. B/op: 1KB 3.9~4.0KB vs 5.7~6.0KB, 5KB 12.2~12.4KB vs 10.0~10.5KB, 1MB 2.0~2.1MB vs 1.0~1.1MB. The 1MB gap is proto.Marshal allocating a fresh payload buffer on every call; every other phase is at parity with gRPC.

### cleanup
```bash
docker system prune -a -f
```

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