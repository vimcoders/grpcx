FROM golang:1.26.5 AS builder
RUN mkdir -p /build
WORKDIR /build
RUN go env -w GOPROXY=https://goproxy.cn,https://mirrors.aliyun.com/goproxy,direct && go env -w GOSUMDB=sum.golang.google.cn
COPY go.mod .
COPY go.sum .
RUN go mod download -x
COPY . .
RUN CGO_ENABLED=0 GOARCH=amd64 GOOS=linux go build -a -o runner ./cmd/grpcx/main.go

FROM m.daocloud.io/gcr.io/distroless/static:nonroot
# 设置东八区
ENV TZ=Asia/Shanghai
# 切换到普通用户
USER nonroot
# 切换到主目录
WORKDIR /app
# 从构建阶段复制二进制文件和配置
COPY --from=builder /build/runner .
COPY --from=ghcr.io/tarampampam/microcheck:1 /bin/portcheck /bin/portcheck
# 设置容器启动命令
ENTRYPOINT ["/app/runner"]