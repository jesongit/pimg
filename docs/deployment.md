# 部署与升级

正式镜像：[posase/pimg](https://hub.docker.com/r/posase/pimg)，当前版本 `v1.0.27`，仅 `linux/amd64`。内置前端、FFmpeg、FFprobe 和 HEIC 预览工具。

## Docker

首次部署可直接使用 README 的[本地存储命令](../README.md#快速开始)。命名卷自动创建；也可挂载自己的目录：

```sh
docker run -d --name pimg --restart unless-stopped \
  -p 127.0.0.1:8787:8787 \
  -v /opt/pimg/data:/pimg/data \
  -v /opt/pimg/runtime:/pimg/runtime \
  -e PIMG_COLD_STORAGE=local \
  -e PIMG_HOT_STORAGE=local \
  posase/pimg:v1.0.27
```

首次默认 `admin/admin`，登录后按提示更换密码。可通过 `PIMG_ADMIN_USERNAME`、`PIMG_ADMIN_PASSWORD` 指定初始账号；已有数据库使用原账号，不会被新环境值重置。

## Compose

```sh
git clone https://github.com/jesongit/pimg.git
cd pimg
cp config.example.yaml config.yaml
```

创建 `.env`：

```dotenv
PIMG_COLD_STORAGE=local
PIMG_HOT_STORAGE=local
```

```sh
docker compose pull pimg
docker compose up -d --no-build
```

默认映射宿主机 `8787`，按环境调整 `ports`。例如只允许本机反向代理时改为 `127.0.0.1:8787:8787`。`/config.yaml` 挂载自项目内的 `config.yaml`，先复制示例，不要把空目录误挂成配置文件。

## 配置与公网访问

站点名称、介绍、公开范围、存储连接、上传限制、临时保留及公网地址在后台管理。`.env.example` 列出初始化及路径设置，已迁移到网页的配置以后台保存值为准。

公网使用 HTTPS 反向代理，在「设置 → 站点信息」填写实际地址。本站永久、临时入口需要 PIMG 保持在线；提供方直接地址只对应具体副本。

需要存储出站代理时在后台连接配置填写代理地址；可参考 `deploy/proxy/config.example.yaml` 创建未入库的实际代理配置，不要将代理凭据提交到 Git。

## 数据目录

| 容器路径 | 内容 | 保留要求 |
| --- | --- | --- |
| `/pimg/data` | 数据库、主密钥、TG 会话、启动配置、本地存储、待完成上传和续传输入 | 必须持久化并备份 |
| `/pimg/runtime` | 缓存、预览、诊断与临时处理文件 | 可重建，升级继续使用原挂载 |
| 额外存储挂载 | 自定义本地实例及导入源 | 按实际配置单独保留 |

新实例主密钥默认位于 `data/system/keys/master.key`；历史实例以实际配置为准。迁移时保留**原密钥**与原 TG 会话，不能生成替代密钥。

## 升级已有实例

先停止并备份数据、外部存储及配置，再复用原部署参数。只拉取或重启旧容器不会切换镜像。

Compose 在原文件改为目标镜像，在原目录使用原项目名及全部 `-f` 参数执行：

```sh
docker compose pull pimg
docker compose up -d --no-deps --no-build pimg
```

`docker run` 部署只替换镜像并重建 PIMG 容器，原挂载、环境变量、端口、网络与素材路径全部沿用。不要使用 `down -v` 或删除原数据卷。

当前沿用 schema 30，从 `v1.0.25` 升级无新增迁移或升级备份。更早版本需要迁移时，先生成并校验内置数据库、密钥、会话和配置备份；失败则停止启动。内置备份不包含全部媒体，不能替代完整备份。

```sh
docker exec pimg /app/pimg version
docker exec pimg wget -qO- http://127.0.0.1:8787/healthz
docker logs --tail 100 pimg
```

通过原网址登录，核对相册、公开范围和原件下载。回退时停止新版并恢复升级前数据、配置及原镜像，不能让旧程序直接连接新版数据库。

## 备份与迁移

在线迁移使用后台导出；手动备份先停机，再复制完整数据目录，包含隐藏文件、SQLite 附属文件、密钥及会话。外部存储和配置单独备份。

同一数据目录或 TG 会话同一时间只运行一个实例。恢复后沿用原路径映射，或按目标容器调整本地路径，保留文件内容。
