# 构建阶段：前端 + 后端
FROM --platform=$BUILDPLATFORM node:22-alpine AS frontend
WORKDIR /app/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci --no-fund --no-audit
COPY frontend/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.26.9-alpine AS backend
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=frontend /app/frontend/dist ./frontend/dist
# Large generated TG schemas can need a higher compiler heap target.
# This build-only setting never changes the service's runtime memory target.
ARG BUILD_GOMEMLIMIT=512MiB
ARG BUILD_TG_GCFLAGS=
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} GOEXPERIMENT=nogreenteagc GOMAXPROCS=2 GOMEMLIMIT=${BUILD_GOMEMLIMIT} go build -p 1 -gcflags="github.com/gotd/td/tg=${BUILD_TG_GCFLAGS}" -trimpath -ldflags="-s -w" -o /pimg ./cmd/pimg

# 运行阶段：单二进制 + ffmpeg/ffprobe（播放兼容、视频切分和封面）
FROM alpine:3.23
LABEL org.opencontainers.image.title="Pimg" \
      org.opencontainers.image.version="1.0.32" \
      org.opencontainers.image.source="https://github.com/jesongit/pimg" \
      org.opencontainers.image.url="https://www.posase.net" \
      org.opencontainers.image.description="Multi-user image and album storage with Telegram archives"
RUN apk add --no-cache ca-certificates tzdata ffmpeg libheif-tools
COPY --from=backend /pimg /app/pimg
WORKDIR /pimg
ENV PIMG_DATA_DIR=/pimg/data PIMG_RUNTIME_DIR=/pimg/runtime
VOLUME ["/pimg/data", "/pimg/runtime"]
EXPOSE 8787
ENTRYPOINT ["/app/pimg"]
