# 开发说明

## 技术栈

- Go 1.26.9、SQLite；入口 `cmd/pimg`。
- Node.js 22、React、TypeScript、Vite；前端位于 `frontend/`。
- FFmpeg、FFprobe；HEIC/HEIF 预览另需 libheif 的 `heif-thumbnailer`。

生产前端通过 `frontend/embed.go` 嵌入 Go 程序，**先构建前端，再编译或测试后端**。

## 本地构建

```sh
git clone https://github.com/jesongit/pimg.git
cd pimg
cp config.example.yaml config.yaml
cd frontend
npm ci
npm run build
cd ..

GOEXPERIMENT=nogreenteagc GOMAXPROCS=2 GOMEMLIMIT=512MiB \
  go build -p 1 -gcflags='github.com/gotd/td/tg=-l' -o pimg ./cmd/pimg

PIMG_COLD_STORAGE=local PIMG_HOT_STORAGE=local ./pimg
```

`nogreenteagc` 沿用运行时兼容设置；`-p 1` 及只对 TG 生成代码关闭内联控制编译内存。软目标不是进程硬上限，需要为编译和媒体处理留足内存与磁盘。

Windows PowerShell 可使用项目脚本：

```powershell
.\pimg.ps1 start
.\pimg.ps1 status
.\pimg.ps1 stop
```

脚本启动后端和 Vite，默认后端 `8790`、前端 `5174`。开发实例不能与正式实例共用数据库或 Telegram 会话。

## 检查

前端执行 `npm ci` 和 `npm run build`。前端构建后，在项目根目录运行：

```sh
GOEXPERIMENT=nogreenteagc GOMAXPROCS=2 GOMEMLIMIT=512MiB \
  go test -p 1 -gcflags='github.com/gotd/td/tg=-l' ./cmd/... ./internal/... ./frontend/...

GOEXPERIMENT=nogreenteagc GOMAXPROCS=2 GOMEMLIMIT=512MiB \
  go vet -p 1 -gcflags='github.com/gotd/td/tg=-l' ./cmd/... ./internal/... ./frontend/...
```

真实提供方测试需显式配置及启用，不属于默认单元测试。截图、演示及开发验证使用独立数据，不复用生产 TG 会话。

## Docker 构建

```sh
docker build \
  --build-arg BUILD_GOMEMLIMIT=512MiB \
  --build-arg BUILD_TG_GCFLAGS=-l \
  -t pimg:local .
```

构建参数只作用于编译。正式镜像使用版本标签，旧版本不覆盖。Dockerfile 的 OCI source/url 元数据指向本仓库和演示站。

## 提交边界

只提交源码、锁文件、脱敏配置示例和公开文档。实际配置、密钥、会话、媒体、数据库、缓存及本地验收记录均不入库。反馈问题时提供版本、环境、复现步骤和移除敏感信息后的日志。
