@echo off
chcp 65001 >nul
cls

go env -w GOPROXY=https://goproxy.cn,https://mirrors.aliyun.com/goproxy,direct && go env -w GOSUMDB=sum.golang.google.cn
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
golangci-lint run --fix ./...
go test -bench "^BenchmarkEcho$" -cpu="4,8,16,20" -benchtime=60s -benchmem -count=30
go test -bench "^BenchmarkStdGRPC_Echo$" -cpu="4,8,16,20" -benchtime=60s -benchmem -count=30
pause
