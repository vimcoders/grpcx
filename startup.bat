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
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0
echo 🛡️ 安装依赖漏洞扫描 govulncheck
go install golang.org/x/vuln/cmd/govulncheck@v1.10.0
echo 🕵️ 安装变量遮蔽检查 shadow
go install golang.org/x/tools/go/analysis/passes/shadow/cmd/shadow@v0.53.0
echo 📦 安装 gRPC 代码生成工具
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
echo ✨ 安装Protobuf格式化工具
go install github.com/bufbuild/buf/cmd/buf@latest
echo 🎨 执行代码自动格式化与规范检测
golangci-lint run --fix ./...
echo 🧾 校验依赖完整性 go mod verify
go mod verify 
echo 📐 校验 go.mod 无漂移 go mod tidy -diff
go mod tidy -diff
echo 🕵️ 变量遮蔽检查 shadow
go vet -vettool=%USERPROFILE%\go\bin\shadow.exe ./... 
echo ✅ 运行单元测试（打乱顺序 + 覆盖率）
go test -shuffle=on -count=1 -cover ./...
echo 🛡️ 扫描依赖与标准库漏洞（退出码 3 表示有命中，不阻断后续流程）
govulncheck ./...
docker compose up -d --build --wait grpc grpcx
docker compose run bench-1k 
docker compose run bench-5k 
docker compose run bench-1024k 
pause
