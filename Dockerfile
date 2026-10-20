# ---- 构建阶段（静态编译，跨平台产出单文件可执行程序）----
# 自 2026-10-19 起不再内嵌 ffmpeg 二进制（原 internal/ffmpeg/assets 已删除）：
# 运行时会依次找 环境变量 VDL_FFMPEG → 数据目录 data/bin/ffmpeg → 系统 PATH，
# 都没有则由用户在「信息」页点按钮从国内镜像按需下载（约 29MB），
# 解压到 /app/videodata/bin/ffmpeg 后直接使用，容器重建也只有 volume 在就一直复用。
# 因此本镜像默认不含 ffmpeg；需要离线/无外网时二选一：
#   1) 运行阶段加 apk add --no-cache ffmpeg；
#   2) 挂载 volume 后手动把 ffmpeg 放到 /app/videodata/bin/ 下。
#
# 注意：builder 的 Go 版本必须 >= go.mod 里声明的版本（当前 1.26），
# 否则构建时会临时下载新工具链，离线环境会直接失败。
# 构建多架构（NAS 常见 amd64 / arm64）：
#   docker buildx build --platform linux/amd64,linux/arm64 -t videodl:latest .
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src
# Go 模块代理：默认走国内镜像。proxy.golang.org 在国内基本连不上（飞牛/群晖构建时
# 会卡在 "dial tcp ...:443: i/o timeout"）。海外机器可以覆盖：
#   docker build --build-arg GOPROXY=https://proxy.golang.org,direct .
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}
# 禁止自动下载新工具链：离线/受限网络下它会再触发一次外网请求，直接把构建拖死
ENV GOTOOLCHAIN=local
COPY go.mod go.sum ./
# 网络抖动重试三次；三次都失败要让构建明确失败（否则会拖到 go build 才报错）
RUN set -eux; \
    ok=0; \
    for i in 1 2 3; do \
      if go mod download; then ok=1; break; fi; \
      echo "go mod download 失败（第 ${i} 次），5 秒后重试..."; \
      sleep 5; \
    done; \
    test "$ok" = "1"
# 前端构建产物已随仓库提交在 internal/web/static（go:embed 打包），
# 这里不需要 Node；要改前端先在本地跑 `make web` 再构建镜像。
COPY . .
RUN set -eux; \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
      go build -trimpath -ldflags="-s -w" -o /out/videodl .

# ---- 运行阶段（极简镜像）----
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
    && cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime \
    && echo "Asia/Shanghai" > /etc/timezone
WORKDIR /app
COPY --from=builder /out/videodl /app/videodl
VOLUME ["/app/videodata"]
EXPOSE 5200
# 默认端口 5200、低配模式（NAS 推荐）。参数可整体覆盖，例如：
#   docker run -p 5200:5200 ... videodl -addr :5200 -data /app/videodata -memlimit 1024
# 日志统一输出到 stdout（单行、带毫秒时间戳），由 docker 的 json-file 驱动负责轮转。
ENV VDL_LOG_LEVEL=info
ENTRYPOINT ["/app/videodl"]
CMD ["-addr", ":5200", "-data", "/app/videodata", "-lowmem"]
