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

## 只读连接诊断

诊断令牌使用 `Authorization: Bearer <TOKEN>` 请求 `/api/diagnostics/status`、`metrics`、`logs`、`activity` 和 `telegram`，不授予管理接口或写操作权限。`/api/diagnostics/telegram` 仅采集本地状态，不触发重新连接、验证码、会话解密、存储探测或额度持久化；账号、群、手机号、API Hash、代理地址和凭据均不返回。

TG 快照区分当前授权状态与底层连接状态，提供 TCP/代理尝试次数、成功次数、在途拨号、最近就绪时间、持续连接起点、最近失败阶段与类型。所有时间戳为 Unix 毫秒，失败历史仅保留最近 16 项。连接已经恢复时，历史失败不会被抹去，应结合 `connected`、`transport_state`、`last_ready_at` 与失败时间判断。TCP 成功但没有进入 ready 时，需继续检查 MTProto 初始化或网络中断，不能直接认定密码失效。连接失败写入 `telegram_connection_failure` 日志，仅记录固定阶段、类型和代理 HTTP 状态码，不记录原始网络错误或 RPC 输入。

Linux 性能采样额外提供进程磁盘实际读写字节、字符 I/O 与主/次缺页累计计数，用相邻采样差值辅助区分页缓存读取、实际磁盘读取和 CPU 工作。单次采样不能替代精确的请求级剖析。

## 页面反馈与原生图片操作

操作结果使用 `useToast()`，声明式的请求错误或带重试的反馈使用 `FloatingFeedback`。两者统一呈现在顶部飘字层，通过 Portal 脱离页面布局；不要为保存、复制或校验结果在表单、底栏追加提示段落。已有的字段帮助、文件／任务记录是页面内容，继续放在对应位置。

飘字在弹窗和全屏中保持可见；F8 聚焦通知，Tab 在可用操作间切换；普通模式下 Esc 关闭当前通知，全屏时 Esc 优先退出全屏。长消息保留详情入口，重试按钮保留原处理函数，上传进度沿用同一通知标识更新。

图片的真实 `img` 元素保留浏览器原生右键和手机长按菜单；透明手势层只接管视频。放大图片的拖动在达到移动阈值后才捕获指针，不取消静止按压。图片复制不可用时只提示，不自动取图或打开复制窗口；需手动复制原图时先在查看器加载原图，再使用浏览器菜单。
