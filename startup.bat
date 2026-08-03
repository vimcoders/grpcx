@echo off
chcp 65001 >nul
cls

echo 🎨 统一代码换行符配置
git config core.autocrlf input
echo 🚀 配置Golang国内代理源
go env -w GOPROXY=https://goproxy.cn,https://mirrors.aliyun.com/goproxy,direct && go env -w GOSUMDB=sum.golang.google.cn
echo 💡 安装代码自动补全 gopls
go install golang.org/x/tools/gopls@latest
echo 🧹 安装代码规范检测 golangci-lint v2
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
echo 📦 安装 gRPC 代码生成工具
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
echo ✨ 安装Protobuf格式化工具
go install github.com/bufbuild/buf/cmd/buf@latest
echo 🎨 执行代码自动格式化与规范检测
golangci-lint run --fix ./...
go test -bench "^BenchmarkEcho$" -cpu="1,2,4,8" -benchtime=90s -benchmem -count=6
go test -bench "^BenchmarkStdGRPC_Echo$" -cpu="1,2,4,8" -benchtime=90s -benchmem -count=6
pause
