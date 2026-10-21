# ============================================================
# 影视库（videodl）极简 Dockerfile
# 阶段 1 编译：golang-alpine 静态编译
# 阶段 2 运行：golang-alpine 基座（保留容器内「立即更新」自编译能力）
# ============================================================
FROM golang:1.26-alpine AS builder

WORKDIR /app

# 先复制依赖文件（利用 Docker 层缓存）
COPY go.mod go.sum ./
COPY *.go ./
COPY internal/ ./internal/
COPY VERSION ./

# 国内构建走 goproxy.cn；海外可 --build-arg GOPROXY=https://proxy.golang.org,direct
ARG GOPROXY=https://goproxy.cn|https://goproxy.io|direct
ENV GOPROXY=${GOPROXY} GOSUMDB=sum.golang.org GOTOOLCHAIN=auto

# 缓存挂载：模块缓存 + 编译缓存放在 BuildKit 缓存里，改代码重建不用重编
# modernc.org/sqlite（首次约 3~4 分钟）。
# ⚠️ 需要 BuildKit（Docker 23+ 默认开启）。老 Docker 报
#    "the --mount option requires BuildKit" / unknown flag 时，删掉两行 --mount 即可。
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download && \
    CGO_ENABLED=0 go build -trimpath \
      -ldflags="-s -w -X videoDownLoad/internal/version.Version=$(cat VERSION)" \
      -o videodl .

# ============================================================
# 阶段 2：运行
# ============================================================
FROM golang:1.26-alpine

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
