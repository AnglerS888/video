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
# 基础镜像默认走国内加速（1panel，实测大层 ~1.2MB/s）。
# 1ms.run 的 manifest 很快但大层走 CloudFront，实测只有 ~29KB/s，不推荐做主力。
# 海外或想用官方可覆盖：
# 例：docker build --build-arg GOLANG_IMAGE=golang:1.26-alpine --build-arg ALPINE_IMAGE=alpine:3.20 .
ARG GOLANG_IMAGE=docker.1panel.live/library/golang:1.26-alpine
ARG ALPINE_IMAGE=docker.1panel.live/library/alpine:3.20

FROM --platform=$BUILDPLATFORM ${GOLANG_IMAGE} AS builder
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src
# Go 模块代理：默认走国内镜像。proxy.golang.org 在国内基本连不上（飞牛/群晖构建时
# 会卡在 "dial tcp ...:443: i/o timeout"）。海外机器可以覆盖：
#   docker build --build-arg GOPROXY=https://proxy.golang.org,direct .
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}
# 版本信息：默认读仓库根目录 VERSION，也可用 --build-arg APP_VERSION=1.4.0 覆盖。
# GIT_COMMIT / BUILD_TIME 用于「信息」页展示与在线更新判断（镜像里没有 .git，
# 所以 commit 只能外部传；不传时更新检查会退化为「提交时间 vs 镜像构建时间」）。
ARG APP_VERSION=
ARG GIT_COMMIT=
ARG BUILD_TIME=
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
    VER="${APP_VERSION:-$(cat VERSION 2>/dev/null || echo dev)}"; \
    CMT="${GIT_COMMIT:-}"; \
    BT="${BUILD_TIME:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"; \
    echo "building version=${VER} commit=${CMT} time=${BT}"; \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
      go build -trimpath \
        -ldflags="-s -w -X videoDownLoad/internal/version.Version=${VER} -X videoDownLoad/internal/version.Commit=${CMT} -X videoDownLoad/internal/version.BuildTime=${BT}" \
        -o /out/videodl .

# ---- 运行阶段（极简镜像）----
FROM ${ALPINE_IMAGE}
# Alpine 软件源：国内 dl-cdn.alpinelinux.org 很慢（apk add 能跑几分钟），默认用阿里云。
# 想用官方源：--build-arg APK_MIRROR=
ARG APK_MIRROR=https://mirrors.aliyun.com/alpine
# 注意：APK_MIRROR 是「带 /alpine 的根地址」，替换时只匹配到 /alpine 为止，
# 否则会拼成 .../alpine/alpine/v3.20/main（实测镜像源直接 404）。
# 镜像源不可用时自动回退官方源，避免构建直接失败。
RUN set -eux; \
    if [ -n "${APK_MIRROR}" ]; then \
      sed -i -e "s#https://dl-cdn\.alpinelinux\.org/alpine#${APK_MIRROR}#g" -e "s#http://dl-cdn\.alpinelinux\.org/alpine#${APK_MIRROR}#g" /etc/apk/repositories; \
      echo "apk 源已切换：${APK_MIRROR}"; \
    fi; \
    if ! apk add --no-cache ca-certificates tzdata; then \
      if [ -n "${APK_MIRROR}" ]; then \
        echo "镜像源不可用，回退官方源重试"; \
        sed -i "s#${APK_MIRROR}#https://dl-cdn.alpinelinux.org/alpine#g" /etc/apk/repositories; \
        apk add --no-cache ca-certificates tzdata; \
      else \
        exit 1; \
      fi; \
    fi; \
    cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime; \
    echo "Asia/Shanghai" > /etc/timezone

WORKDIR /app
COPY --from=builder /out/videodl /app/videodl
VOLUME ["/app/videodata"]
EXPOSE 5200
# 配置全部来自 config.yml（没有命令行参数）；页面可改的项在同目录 ui.yml。
# 默认把配置放在 volume 上：首次启动自动生成，之后改配置只需改这个文件并重启容器。
ENV VDL_CONFIG=/app/videodata/config.yml
# 日志统一输出到 stdout（单行、带毫秒时间戳），由 docker 的 json-file 驱动负责轮转。
ENTRYPOINT ["/app/videodl"]
