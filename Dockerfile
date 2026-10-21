# ---- 构建阶段（静态编译，跨平台产出单文件可执行程序）----
# 自 2026-10-19 起不再内嵌 ffmpeg 二进制（原 internal/ffmpeg/assets 已删除）：
# 运行时会依次找 环境变量 VDL_FFMPEG → 数据目录 data/bin/ffmpeg → 系统 PATH，
# 都没有则由用户在「信息」页点按钮从国内镜像按需下载（约 29MB），
# 解压到 /app/videodata/bin/ffmpeg 后直接使用，容器重建也只有 volume 在就一直复用。
# 因此本镜像默认不含 ffmpeg；需要离线/无外网时二选一：
#   1) 运行阶段加 apk add --no-cache ffmpeg；
#   2) 挂载 volume 后手动把 ffmpeg 放到 /app/videodata/bin/ 下。
#
# ===================== 国内构建链路（重点） =====================
# 1) 基础镜像：走国内镜像站，避免直连 Docker Hub（国内基本连不上/被限流）。
#    默认 1panel（实测大层 ~1.2MB/s）；1ms.run 的 manifest 很快但大层走 CloudFront，
#    实测只有 ~29KB/s，不推荐做主力。可用 --build-arg 覆盖：
#      docker build --build-arg GOLANG_IMAGE=golang:1.26-alpine .
#    国内可用的候选（任选其一，均已实测可达）：
#      docker.1panel.live / docker.1ms.run / docker.m.daocloud.io / docker.amingg.com
#    ⚠️ docker.xuanyuan.me / docker.nju.edu.cn 实测返回裸 403（没有 WWW-Authenticate），
#       镜像站不可用，不要放在首选位置。
# 2) Go 模块：走 goproxy.cn 等国内代理（见 GOPROXY）。
# 3) 校验数据库：GOSUMDB=sum.golang.org（**不再默认 off**）。
#    Go 会把 sumdb 查询**转发给 GOPROXY 里的代理**（<代理>/sumdb/sum.golang.org/lookup/...），
#    实测 goproxy.cn / mirrors.aliyun.com / goproxy.io 都返回 200，所以国内可用。
#    这样既保留完整哈希校验，又允许 go 自动补齐工具链（golang.org/toolchain 必须在 sumdb 校验，
#    GOSUMDB=off 会直接报 "checksum database disabled by GOSUMDB=off" 导致构建失败）。
# 4) Alpine 装包：走阿里云 / 清华源（见 APK_MIRROR）。
# ============================================================
ARG GOLANG_IMAGE=docker.1panel.live/library/golang:1.26-alpine
# 运行阶段基座（带 Go 工具链）。⚠️ 必须声明在**第一个 FROM 之前**：
# 写在两个 FROM 之间的 ARG 只属于上一个阶段，后面的 FROM 看不到，会报
# "base name (${RUNTIME_IMAGE}) should not be blank"。
# 想换回精简小镜像（在线更新退回「下载源码+宿主机重建」）：
#   docker build --build-arg RUNTIME_IMAGE=docker.1panel.live/library/alpine:3.20 .
ARG RUNTIME_IMAGE=docker.1panel.live/library/golang:1.26-alpine

# ---- 构建阶段 ----
# 这里**故意不写** `--platform=$BUILDPLATFORM`：
#   - BUILDPLATFORM 是 BuildKit 独有的自动变量，老 Docker（20.10 的 classic builder，
#     群晖/飞牛 NAS 上很常见）根本没有它，展开成空串后 platforms.Parse("") 直接报错，
#     整条构建挂掉；
#   - 去掉之后 FROM 走默认平台：BuildKit 取目标平台（--platform 指定的那个），
#     classic builder 取宿主架构 —— 正好都是我们想要的。
# 多架构（NAS 常见 amd64 / arm64）：
#   docker buildx build --platform linux/amd64,linux/arm64 -t videodl:latest .
#   （跨架构需要宿主机装 QEMU：docker run --privileged --rm tonistiigi/binfmt --install all）
FROM ${GOLANG_IMAGE} AS builder
# 目标平台：BuildKit 会自动注入 TARGETOS / TARGETARCH（对应 --platform）。
# ⚠️ 这里**绝对不能写默认值**（例如 ARG TARGETARCH=amd64）：
#    有默认值就会盖掉 BuildKit 注入的目标架构，--platform linux/arm64 依然编出 amd64
#    二进制，ARM 机器上容器直接起不来（exec format error）。
# 老 Docker（classic builder）没有这两个变量，保持为空即可 ——
# 下面的编译步骤会用 `go env GOARCH / GOOS` 回落到宿主架构，行为正确。
ARG TARGETOS
ARG TARGETARCH
# Go 模块代理（国内链路）：
#   逗号分隔 = 前一个返回 404/410 才换下一个；
#   竖线分隔 = 前一个**出错**（超时/DNS 抖动/连接失败）也立刻换下一个 —— 国内网络抖，必须用竖线。
#   顺序：goproxy.cn → 阿里云 → 腾讯云 → goproxy.io → direct。
#   这几家同时代理了 sumdb 端点，所以 GOSUMDB=sum.golang.org 在国内也能查到。
# 海外机器可以覆盖：
#   docker build --build-arg GOPROXY=https://proxy.golang.org,direct .
ARG GOPROXY=https://goproxy.cn|https://mirrors.aliyun.com/goproxy/|https://mirrors.cloud.tencent.com/go/|https://goproxy.io|direct
# 校验数据库：见文件头说明。sum.golang.org 本身国内连不上，但查询会经 GOPROXY 转发，
# 所以保留完整哈希校验的同时还能让 go 自动下载缺失的工具链。
ARG GOSUMDB=sum.golang.org
# 工具链策略：auto = 基础镜像的 Go 版本低于 go.mod 要求时，go 会通过 GOPROXY 自动下载
# 对应版本的工具链（国内代理实测可下，约 70MB，一次性开销）。
# 离线/完全不能出网的环境可以改成 local：此时版本不够会**立刻失败**并给出可操作提示。
ARG GOTOOLCHAIN=auto
ENV GOPROXY=${GOPROXY} \
    GOSUMDB=${GOSUMDB} \
    GOTOOLCHAIN=${GOTOOLCHAIN}
# 版本信息：默认读仓库根目录 VERSION，也可用 --build-arg APP_VERSION=1.4.0 覆盖。
# GIT_COMMIT / BUILD_TIME 用于「信息」页展示与在线更新判断（镜像里没有 .git，
# 所以 commit 只能外部传；不传时更新检查会退化为「提交时间 vs 镜像构建时间」）。
ARG APP_VERSION=
ARG GIT_COMMIT=
ARG BUILD_TIME=
# 先把依赖清单放进镜像（后面 go mod download / go build 都要用）
COPY go.mod go.sum ./
# 校验工具链版本是否满足 go.mod（国内镜像站的 golang 镜像偶尔会滞后）
#   - GOTOOLCHAIN=local：版本不够直接失败，并提示两条可选解法；
#   - GOTOOLCHAIN=auto ：只告警，随后 go 会自己下载对应版本工具链继续。
# 用 `go version` 而不是 `go env GOVERSION` 取版本号：`go env` 会读 go.mod，
# 当 go.mod 要求更高版本且 GOTOOLCHAIN=local 时它会直接报错退出，
# 根本走不到下面这段友好提示。
RUN set -eux; \
    want="$(awk '/^go /{print $2; exit}' go.mod)"; \
    have="$(go version | awk '{print $3}' | sed 's/^go//')"; \
    case "$have" in [0-9]*) ;; *) have="" ;; esac; \
    echo "go.mod 需要 go ${want}，基础镜像自带 go${have:-devel}"; \
    if [ -n "$have" ] && ! awk -v a="$have" -v b="$want" 'BEGIN{ split(a,x,"."); split(b,y,"."); \
        if (x[1]+0 != y[1]+0) exit (x[1]+0 > y[1]+0)?0:1; \
        exit (x[2]+0 >= y[2]+0)?0:1 }'; then \
      if [ "${GOTOOLCHAIN}" = "local" ]; then \
        echo "ERROR: 基础镜像的 Go 版本过低（需要 go${want}，实际 go${have}）。"; \
        echo "       换更新的 GOLANG_IMAGE，或加 --build-arg GOTOOLCHAIN=auto 让 go 自动下载工具链。"; \
        exit 1; \
      fi; \
      echo "WARN: 基础镜像 Go 版本偏低，将通过 GOPROXY 自动下载 go${want} 工具链（首次约 70MB，国内代理实测可用）"; \
    fi
# 网络抖动重试三次；三次都失败要让构建明确失败（否则会拖到 go build 才报错）。
# GOPROXY 里的竖线已经保证「一个代理挂了就换下一个」，这里的重试是兜底 DNS 整体抽风。
RUN set -eux; \
    ok=0; \
    for i in 1 2 3; do \
      if go mod download; then ok=1; break; fi; \
      echo "go mod download 失败（第 ${i}/3 次），10 秒后重试..."; \
      sleep 10; \
    done; \
    test "$ok" = "1"
# 前端构建产物已随仓库提交在 internal/web/static（go:embed 打包），
# 这里不需要 Node；要改前端先在本地跑 `make web` 再构建镜像。
COPY . .
# 编译：目标平台优先用 TARGETOS/TARGETARCH（BuildKit 自动注入），
# 拿不到（老 Docker / 未指定）就回落到当前镜像自身的架构（go env GOARCH）——
# 也就是「给谁构建就出谁的架构」，不会编出错架构的二进制。
RUN set -eux; \
    VER="${APP_VERSION:-$(cat VERSION 2>/dev/null || echo dev)}"; \
    CMT="${GIT_COMMIT:-}"; \
    BT="${BUILD_TIME:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"; \
    OS="${TARGETOS:-$(go env GOOS)}"; \
    ARCH="${TARGETARCH:-$(go env GOARCH)}"; \
    echo "building version=${VER} commit=${CMT} time=${BT} target=${OS}/${ARCH}"; \
    CGO_ENABLED=0 GOOS="$OS" GOARCH="$ARCH" \
      go build -trimpath \
        -ldflags="-s -w -X videoDownLoad/internal/version.Version=${VER} -X videoDownLoad/internal/version.Commit=${CMT} -X videoDownLoad/internal/version.BuildTime=${BT}" \
        -o /out/videodl .

# ---- 运行阶段（带 Go 工具链：容器内一键更新）----
# 自 2026-10-21 起运行基座从纯 alpine 换成 golang（镜像约 400MB，比原来大 ~350MB）：
# 换来 NAS 上「信息」页点一次「立即更新」，就能在容器内下载源码 → go build →
# 替换二进制 → 自动重启（靠 restart: unless-stopped），不用再登录宿主机重建镜像。
# 编译用的模块/构建缓存放在数据目录（volume），容器重建不丢，下次更新只增量编译。
# 介意体积可以切回精简镜像，在线更新会自动退回「下载源码 + 宿主机重建」的老模式：
#   docker build --build-arg RUNTIME_IMAGE=docker.1panel.live/library/alpine:3.20 .
# RUNTIME_IMAGE 已在文件顶部（第一个 FROM 之前）声明，这里直接用；重复声明在这里无效。
FROM ${RUNTIME_IMAGE}
# 容器内在线更新时 go build 用的代理：跟随构建参数 GOPROXY，
# 不传就用国内镜像（竖线串联多个国内代理 + direct 兜底）；运行时也能用 `-e GOPROXY=...` 覆盖。
ARG GOPROXY=https://goproxy.cn|https://mirrors.aliyun.com/goproxy/|https://mirrors.cloud.tencent.com/go/|https://goproxy.io|direct
# 同上：容器内 go build 也走国内代理；sumdb 查询同样经代理转发，所以校验库不用关。
ARG GOSUMDB=sum.golang.org
# 工具链：auto 让容器内更新在基础镜像 Go 版本偏旧时也能自动补齐（经 GOPROXY 下载）。
ARG GOTOOLCHAIN=auto
ENV GOPROXY=${GOPROXY} \
    GOSUMDB=${GOSUMDB} \
    GOTOOLCHAIN=${GOTOOLCHAIN}
# Alpine 软件源：国内 dl-cdn.alpinelinux.org 很慢（apk add 能跑几分钟），默认用阿里云。
# 想用官方源：--build-arg APK_MIRROR=
ARG APK_MIRROR=https://mirrors.aliyun.com/alpine
# 备用源：阿里云抽风时依次尝试（清华源实测可用）
ARG APK_FALLBACK_MIRRORS=https://mirrors.tuna.tsinghua.edu.cn/alpine
# 注意：APK_MIRROR 是「带 /alpine 的根地址」，替换时只匹配到 /alpine 为止，
# 否则会拼成 .../alpine/alpine/v3.20/main（实测镜像源直接 404）。
# ⚠️ 这一步是「尽力而为」，**绝不允许让构建失败**：
#   - 时区库已编进二进制（main.go 的 `_ "time/tzdata"`），不需要 apk 装 tzdata；
#   - CA 证书 golang 官方镜像自带（/etc/ssl/certs/ca-certificates.crt）；
#   - NAS/VM 上 apk 镜像源的 DNS 经常抖（2026-10-21 实测 aliyun 与官方源同时
#     "DNS: transient error"，回退也救不了，直接把构建卡死在这里）。
RUN set -eux; \
    if command -v apk >/dev/null 2>&1; then \
      need=""; \
      [ -s /etc/ssl/certs/ca-certificates.crt ] || need="${need} ca-certificates"; \
      [ -f /usr/share/zoneinfo/Asia/Shanghai ] || need="${need} tzdata"; \
      if [ -n "${need}" ]; then \
        for m in ${APK_MIRROR} ${APK_FALLBACK_MIRRORS}; do \
          [ -n "$m" ] || continue; \
          sed -i -e "s#https://dl-cdn\.alpinelinux\.org/alpine#${m}#g" -e "s#http://dl-cdn\.alpinelinux\.org/alpine#${m}#g" /etc/apk/repositories || true; \
          echo "尝试 apk 源：${m}"; \
          if apk add --no-cache ${need}; then break; fi; \
          echo "WARN: 源 ${m} 不可用，换下一个"; \
        done; \
        echo "WARN: apk 装证书/时区失败，继续构建（时区已内置；证书缺失会影响 HTTPS）"; \
      else \
        echo "证书与时区已就绪，跳过 apk"; \
      fi; \
    fi; \
    if [ -f /usr/share/zoneinfo/Asia/Shanghai ]; then \
      cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime; \
    fi; \
    echo "Asia/Shanghai" > /etc/timezone

WORKDIR /app
COPY --from=builder /out/videodl /app/videodl
VOLUME ["/app/videodata"]
EXPOSE 5200
# 配置全部来自 config.yml（没有命令行参数）；页面可改的项在同目录 ui.yml。
# 默认把配置放在 volume 上：首次启动自动生成，之后改配置只需改这个文件并重启容器。
ENV VDL_CONFIG=/app/videodata/config.yml
LABEL org.opencontainers.image.title="videodl" \
      org.opencontainers.image.description="影视库：采集、下载、刮削、播放" \
      org.opencontainers.image.source="https://gitee.com/anglers/video"
# 日志统一输出到 stdout（单行、带毫秒时间戳），由 docker 的 json-file 驱动负责轮转。
# 优雅退出：给正在跑的 ffmpeg 合并/分片下载留出收尾时间。
STOPSIGNAL SIGTERM
# 轻量探活（只 SELECT 1），不要用 /api/system（会扫全表 COUNT）
HEALTHCHECK --interval=60s --timeout=5s --start-period=20s --retries=3 \
  CMD wget -qO- http://127.0.0.1:5200/healthz >/dev/null 2>&1 || exit 1
ENTRYPOINT ["/app/videodl"]
