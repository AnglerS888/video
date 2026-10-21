# ============================================================
# 影视库（videodl）极简 Dockerfile
# 阶段 1 编译：golang-alpine 静态编译
# 阶段 2 运行：golang-alpine 基座（保留容器内「立即更新」自编译能力）
# 兼容性：不依赖 BuildKit / buildx，老版 Docker 也能直接 docker build。
# ============================================================
FROM golang:1.26-alpine AS builder

# alpine 官方源（dl-cdn）在国内经常超时/DNS 抖动，默认换阿里云镜像；
# 海外构建可 --build-arg APK_MIRROR=dl-cdn.alpinelinux.org
ARG APK_MIRROR=mirrors.aliyun.com
# git 必装：GOPROXY 列表用 "|" 分隔时，代理只要出错（超时/DNS/404）就会回退到 direct，
# direct 让 go 直连 VCS 拉模块，而 golang:alpine 基础镜像不带 git，
# 缺它会报 "exec: git: executable file not found in $PATH"。
# ca-certificates：代理走 HTTPS 需要根证书（官方镜像自带，显式装上更稳）。
RUN sed -i "s|dl-cdn.alpinelinux.org|${APK_MIRROR}|g" /etc/apk/repositories && \
    apk add --no-cache git ca-certificates

WORKDIR /app

# 国内构建走 goproxy.cn；海外可 --build-arg GOPROXY=https://proxy.golang.org,direct
# 注意分隔符："|" = 前一个出错就换下一个（含超时）；"," = 只在 404/410 时才换下一个。
ARG GOPROXY=https://goproxy.cn|https://goproxy.io|direct
ENV GOPROXY=${GOPROXY} GOSUMDB=sum.golang.org GOTOOLCHAIN=auto

# 先只拷 go.mod/go.sum 下依赖：之后改 *.go 不会让这一层缓存失效
COPY go.mod go.sum ./

# 不依赖 BuildKit：老 Docker（无 buildx）不认 --mount=type=cache，会直接报
# "the --mount option requires BuildKit" 构建失败。模块缓存改靠 Docker 层缓存：
# go.mod/go.sum 不变时，这一层直接命中，不会重新下载。
RUN go mod download

COPY *.go ./
COPY internal/ ./internal/
COPY VERSION ./

# -buildvcs=false：构建上下文/容器内源码没有 .git 时不必探测版本控制
RUN CGO_ENABLED=0 go build -trimpath -buildvcs=false \
      -ldflags="-s -w -X videoDownLoad/internal/version.Version=$(cat VERSION)" \
      -o videodl .

# ============================================================
# 阶段 2：运行
# ============================================================
FROM golang:1.26-alpine

ARG APK_MIRROR=mirrors.aliyun.com
# 容器内「立即更新」会在镜像里执行 go build，代理失败同样会回退 direct，
# 所以运行时基座也要有 git，否则首次更新就报 git 找不到。
RUN sed -i "s|dl-cdn.alpinelinux.org|${APK_MIRROR}|g" /etc/apk/repositories && \
    apk add --no-cache git ca-certificates

# 时区已编进二进制（main.go 的 _ "time/tzdata"），无需 tzdata
ENV TZ=Asia/Shanghai

WORKDIR /app

COPY --from=builder /app/videodl .

VOLUME ["/app/videodata"]
EXPOSE 5200

# 配置全部来自 config.yml（数据目录下，首次启动自动生成）
ENV VDL_CONFIG=/app/videodata/config.yml
# 容器内在线更新时 go build 用的代理
ENV GOPROXY=https://goproxy.cn|https://goproxy.io|direct

STOPSIGNAL SIGTERM
ENTRYPOINT ["/app/videodl"]
