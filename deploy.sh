#!/usr/bin/env bash
# ============================================================
# 影视库（videodl）Linux 一键部署 / 更新脚本
#
# 做四件事：
#   1. 没有 Docker 就自动安装并启动；
#   2. 从 Gitee 拉最新源码（有 git 用 git pull，没有则下 zip），构建 docker 镜像；
#   3. 询问（或按参数使用）宿主端口和数据存储路径，启动容器；
#   4. 已经有 videodl 容器时，重建镜像并用原配置重新创建容器（= 更新）。
#
# 用法：
#   sudo ./deploy.sh                     # 交互式：问端口、数据目录
#   sudo ./deploy.sh --yes               # 全部用默认值 / 沿用已有容器配置
#   sudo ./deploy.sh --port 8080 --data /data/videodl
#   ./deploy.sh --dry-run                # 只打印将要执行的命令，不安装、不构建
#   ./deploy.sh --help
#
# 说明：容器内配置在 <数据目录>/config.yml，首次启动自动生成；
#       更新只会重建镜像和容器，数据目录（含视频、数据库、配置）不受影响。
# ============================================================
set -Eeuo pipefail

# ---------- 可调参数（也可用同名环境变量） ----------
IMAGE_NAME="${IMAGE_NAME:-videodl:latest}"
IMAGE_PREV="${IMAGE_PREV:-videodl:previous}"
CONTAINER_NAME="${CONTAINER_NAME:-videodl}"
GITEE_REPO="${GITEE_REPO:-anglers/video}"
GITEE_BRANCH="${GITEE_BRANCH:-master}"
DEFAULT_PORT="${DEFAULT_PORT:-5200}"
DEFAULT_DATA="${DEFAULT_DATA:-}"          # 空 = 脚本目录下的 videodata
SOURCE_DIR="${SOURCE_DIR:-}"       # 空 = 脚本目录（有 go.mod 时）否则 ~/videodl-src
GOPROXY_ARG="${GOPROXY_ARG:-https://goproxy.cn,direct}"
# 基础镜像仓库：默认 1panel（实测大层 ~1.2MB/s）；1ms.run 的 manifest 很快但
# 大层走 CloudFront，实测只有 ~29KB/s（用户那边 ~5KB/s），所以只作自动兜底。
REGISTRY="${REGISTRY:-docker.1panel.live}"
REGISTRY_FALLBACK="${REGISTRY_FALLBACK:-docker.1ms.run}"
# Alpine 软件源：默认阿里云，避免 apk add 卡在 dl-cdn.alpinelinux.org
APK_MIRROR="${APK_MIRROR:-https://mirrors.aliyun.com/alpine}"

PORT=""
DATA_DIR=""
ASSUME_YES=0
DRY_RUN=0
NO_INSTALL=0
REGISTRY_MIRROR=""
FORCE=0

# ---------- 输出 ----------
if [ -t 1 ]; then
  C_RED=$'\033[31m'; C_GRN=$'\033[32m'; C_YEL=$'\033[33m'; C_BLU=$'\033[36m'; C_OFF=$'\033[0m'
else
  C_RED=""; C_GRN=""; C_YEL=""; C_BLU=""; C_OFF=""
fi
info() { printf '%s[信息]%s %s\n' "$C_BLU" "$C_OFF" "$*"; }
ok()   { printf '%s[完成]%s %s\n' "$C_GRN" "$C_OFF" "$*"; }
warn() { printf '%s[注意]%s %s\n' "$C_YEL" "$C_OFF" "$*"; }
die()  { printf '%s[错误]%s %s\n' "$C_RED" "$C_OFF" "$*" >&2; exit 1; }
step() { printf '\n%s==> %s%s\n' "$C_GRN" "$*" "$C_OFF"; }

usage() {
  cat <<'EOF'
影视库（videodl）Linux 一键部署 / 更新脚本

用法：
  sudo ./deploy.sh                     交互式：询问端口与数据目录
  sudo ./deploy.sh --yes               全部用默认值；已有容器则沿用其配置
  sudo ./deploy.sh --port 8080 --data /data/videodl
  ./deploy.sh --dry-run                只打印将执行的命令（不安装、不构建、不启动）

参数：
  --port N              宿主机端口（容器内固定 5200）
  --data PATH           数据存储路径（数据库、视频、config.yml 都在这里）
  --source-dir PATH     源码目录（默认脚本目录；没有源码则放 ~/videodl-src）
  --registry HOST       基础镜像仓库（默认 docker.1panel.live；传空用官方）
  --registry-fallback H 备用仓库（默认 docker.1ms.run，主仓库不可达/构建失败时自动切换）
  --apk-mirror URL      Alpine 软件源（默认 https://mirrors.aliyun.com/alpine）
  --goproxy URL         Go 模块代理（默认 https://goproxy.cn,direct）
  --official            全部切回官方源（海外机器用：Docker Hub + Alpine CDN + proxy.golang.org）
  --registry-mirror URL 额外给 Docker daemon 配镜像加速（会重启 docker，默认不改）
  --yes, -y             不交互，使用默认值 / 沿用已有容器
  --no-install          不自动安装 Docker（已装好时用）
  --dry-run             演练模式：只打印命令
  --force               跳过平台 / root 检查（仅调试用）
  -h, --help            显示本帮助

再执行一次脚本 = 从 Gitee 拉最新代码、重建镜像并替换容器（数据目录不受影响）。
EOF
  exit 0
}

# ---------- 参数解析 ----------
ORIG_ARGS="$*"
while [ $# -gt 0 ]; do
  case "$1" in
    --port)            PORT="${2:-}"; shift 2 ;;
    --data)            DATA_DIR="${2:-}"; shift 2 ;;
    --source-dir)      SOURCE_DIR="${2:-}"; shift 2 ;;
    --registry-mirror) REGISTRY_MIRROR="${2:-}"; shift 2 ;;
    --apk-mirror)      APK_MIRROR="${2:-}"; shift 2 ;;
    --registry)          REGISTRY="${2:-}"; shift 2 ;;
    --registry-fallback) REGISTRY_FALLBACK="${2:-}"; shift 2 ;;
    --official)        REGISTRY=""; APK_MIRROR=""; GOPROXY_ARG="https://proxy.golang.org,direct"; shift ;;
    --goproxy)         GOPROXY_ARG="${2:-}"; shift 2 ;;
    --yes|-y)          ASSUME_YES=1; shift ;;
    --dry-run)         DRY_RUN=1; shift ;;
    --no-install)      NO_INSTALL=1; shift ;;
    --force)           FORCE=1; shift ;;
    -h|--help)         usage ;;
    *) die "未知参数：$1（用 --help 看用法）" ;;
  esac
done

# ---------- 基础检查 ----------
# 管道执行（curl ... | bash）时 BASH_SOURCE 未定义：退回当前目录，
# 否则 set -u 会在 bash 4.2 上直接报「BASH_SOURCE[0]: 为绑定变量」
SELF="${BASH_SOURCE[0]:-}"
if [ -z "$SELF" ] || [ "$SELF" = "bash" ] || [ "$SELF" = "-" ] || [ "$SELF" = "/dev/stdin" ]; then
  SCRIPT_DIR="$(pwd)"
  PIPED=1
else
  SCRIPT_DIR="$(cd "$(dirname "$SELF")" && pwd)"
  PIPED=0
fi

# 端口/数据目录必须由用户输入：管道执行时没有终端，直接拒绝（除非显式 --yes 全默认）
if [ "$PIPED" = "1" ] && [ "$ASSUME_YES" != "1" ]; then
  die "从管道执行时无法交互输入端口和数据目录。请先下载脚本再执行：
  curl -fsSL https://gitee.com/${GITEE_REPO}/raw/${GITEE_BRANCH}/deploy.sh -o deploy.sh
  chmod +x deploy.sh && sudo ./deploy.sh
或者显式指定：sudo bash -s -- --yes --port 5200 --data /opt/videodata"
fi


need_root() {
  [ "$DRY_RUN" = "1" ] && return 0
  if [ "$FORCE" = "1" ]; then
    warn "已用 --force 跳过 root 检查（仅供调试）"
    return 0
  fi
  if [ "$(id -u)" -ne 0 ]; then
    die "请用 root 执行：sudo $0 ${ORIG_ARGS}"
  fi
}

# 在 Windows/Git Bash 上只允许 --dry-run，方便先看流程
check_platform() {
  case "$(uname -s)" in
    Linux) : ;;
    *)
      if [ "$DRY_RUN" = "1" ]; then
        warn "当前不是 Linux（$(uname -s)），仅以 --dry-run 方式演示流程"
      elif [ "$FORCE" = "1" ]; then
        warn "非 Linux 平台，已用 --force 继续（自行承担风险）"
      else
        die "本脚本只支持 Linux（当前 $(uname -s)）。Windows/macOS 请参考 README 用 docker compose 部署。"
      fi
      ;;
  esac
}

run() {
  if [ "$DRY_RUN" = "1" ]; then
    printf '%s  + %s%s\n' "$C_YEL" "$*" "$C_OFF"
    return 0
  fi
  "$@"
}

# 判断命令是否存在
has() { command -v "$1" >/dev/null 2>&1; }

# ---------- 系统识别 ----------
OS_ID=""; OS_LIKE=""; PKG=""
detect_os() {
  if [ -r /etc/os-release ]; then
    # shellcheck disable=SC1091
    . /etc/os-release
    OS_ID="${ID:-}"; OS_LIKE="${ID_LIKE:-}"
  fi
  case " $OS_ID $OS_LIKE " in
    *debian*|*ubuntu*) PKG=apt ;;
    *fedora*|*rhel*|*centos*|*rocky*|*almalinux*|*amzn*) PKG=dnf ;;
    *alpine*) PKG=apk ;;
    *suse*|*opensuse*) PKG=zypper ;;
    *) PKG="" ;;
  esac
  # 没有 dnf 的旧 CentOS 退回 yum
  if [ "$PKG" = "dnf" ] && ! has dnf; then PKG=yum; fi
  info "系统：${PRETTY_NAME:-$(uname -s)}（包管理器：${PKG:-未知}）"
}

install_pkg() {
  # install_pkg <包名...>
  case "$PKG" in
    apt)    run env DEBIAN_FRONTEND=noninteractive apt-get update -qq
            run env DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends "$@" ;;
    dnf)    run dnf install -y "$@" ;;
    yum)    run yum install -y "$@" ;;
    apk)    run apk add --no-cache "$@" ;;
    zypper) run zypper --non-interactive install -y "$@" ;;
    *)      die "无法识别包管理器，请手动安装：$*" ;;
  esac
}

# ---------- Docker 安装 ----------
docker_ready() {
  has docker || return 1
  docker info >/dev/null 2>&1
}

start_docker_service() {
  if has systemctl; then
    run systemctl enable --now docker
  elif has service; then
    run service docker start
  elif has rc-service; then
    run rc-update add docker default
    run rc-service docker start
  fi
}

install_docker() {
  step "没有可用的 Docker，开始安装"
  if [ "$DRY_RUN" = "1" ] && [ -z "$PKG" ]; then
    warn "演练模式且无法识别包管理器：真实执行时会尝试官方脚本 get.docker.com，失败则用发行版软件包"
    return 0
  fi
  case "$PKG" in
    apt)
      install_pkg ca-certificates curl gnupg
      # 优先官方脚本；失败则用发行版自带包（版本旧但够用）
      if [ "$DRY_RUN" = "1" ] || curl -fsSL --max-time 20 https://get.docker.com -o /tmp/get-docker.sh; then
        if [ "$DRY_RUN" = "1" ]; then
          run sh /tmp/get-docker.sh
        else
          run sh /tmp/get-docker.sh || { warn "官方脚本失败，改用发行版软件包"; install_pkg docker.io; }
        fi
      else
        warn "下载官方安装脚本失败，改用发行版软件包 docker.io"
        install_pkg docker.io
      fi
      ;;
    dnf|yum)
      install_pkg curl
      if [ "$DRY_RUN" = "1" ]; then
        run sh -c 'curl -fsSL https://get.docker.com | sh'
      else
        curl -fsSL --max-time 20 https://get.docker.com -o /tmp/get-docker.sh \
          && run sh /tmp/get-docker.sh || { warn "官方脚本失败，改用发行版软件包"; install_pkg docker; }
      fi
      ;;
    apk)    install_pkg docker docker-cli ;;
    zypper) install_pkg docker ;;
    *)      die "无法自动安装 Docker，请先手动安装后重试（或加 --no-install 跳过检查）" ;;
  esac
  rm -f /tmp/get-docker.sh
  start_docker_service
  sleep 2
  if [ "$DRY_RUN" != "1" ] && ! docker_ready; then
    die "Docker 安装后仍不可用，请手动检查：systemctl status docker"
  fi
  ok "Docker 已就绪"
}

configure_registry_mirror() {
  # 国内拉不到镜像时用：把镜像加速写进 daemon.json
  [ -n "$REGISTRY_MIRROR" ] || return 0
  step "配置 Docker 镜像加速：$REGISTRY_MIRROR"
  local f=/etc/docker/daemon.json
  run mkdir -p /etc/docker
  if [ "$DRY_RUN" = "1" ]; then
    printf '%s  + 写入 %s: {"registry-mirrors":["%s"]}%s\n' "$C_YEL" "$f" "$REGISTRY_MIRROR" "$C_OFF"
  else
    [ -f "$f" ] && cp -a "$f" "$f.bak.$(date +%s)"
    cat >"$f" <<EOF
{
  "registry-mirrors": ["$REGISTRY_MIRROR"]
}
EOF
    if has systemctl; then run systemctl restart docker; else run service docker restart; fi
    sleep 2
  fi
}

# ---------- 已有容器 / 参数默认值 ----------
existing_container() { has docker && docker ps -a --format '{{.Names}}' 2>/dev/null | grep -qx "$CONTAINER_NAME"; }

container_port() {
  docker port "$CONTAINER_NAME" 5200/tcp 2>/dev/null | head -n1 | awk -F: '{print $NF}'
}

container_data() {
  docker inspect -f '{{range .Mounts}}{{if eq .Destination "/app/videodata"}}{{.Source}}{{end}}{{end}}' \
    "$CONTAINER_NAME" 2>/dev/null || true
}

ask() {
  # ask <提示> <默认值> -> 结果写入 REPLY_VALUE
  local prompt="$1" def="${2:-}" ans=""
  if [ "$ASSUME_YES" = "1" ] || [ ! -t 0 ]; then
    REPLY_VALUE="$def"; return 0
  fi
  if [ -n "$def" ]; then
    read -r -p "$prompt（直接回车用默认）[$def]: " ans || true
    REPLY_VALUE="${ans:-$def}"
  else
    read -r -p "$prompt: " ans || true
    REPLY_VALUE="$ans"
  fi
}

validate_port() {
  case "$1" in
    ''|*[!0-9]*) return 1 ;;
  esac
  [ "$1" -ge 1 ] && [ "$1" -le 65535 ]
}

port_in_use() {
  # 端口被别的进程占用（不是我们自己的容器）时返回 0
  if has ss; then
    ss -ltn 2>/dev/null | awk '{print $4}' | grep -qE "[:.]$1\$"
  elif has netstat; then
    netstat -ltn 2>/dev/null | awk '{print $4}' | grep -qE "[:.]$1\$"
  else
    return 1
  fi
}

resolve_settings() {
  local dport="$DEFAULT_PORT" ddata="$DEFAULT_DATA"
  if existing_container; then
    local cp cd
    cp="$(container_port)"; cd="$(container_data)"
    [ -n "$cp" ] && dport="$cp"
    [ -n "$cd" ] && ddata="$cd"
    info "检测到已有容器 ${CONTAINER_NAME}（端口 ${dport:-?}，数据目录 ${ddata:-?}），本次将重建它"
  fi
  [ -z "$ddata" ] && ddata="$SCRIPT_DIR/videodata"

  step "配置"
  if [ -z "$PORT" ]; then
    ask "宿主机端口（容器内固定 5200）" "$dport"; PORT="$REPLY_VALUE"
  fi
  validate_port "$PORT" || die "端口不合法：$PORT（应为 1~65535）"

  if [ -z "$DATA_DIR" ]; then
    ask "数据存储路径（数据库/视频/配置都在这里）" "$ddata"; DATA_DIR="$REPLY_VALUE"
  fi
  [ -n "$DATA_DIR" ] || die "数据目录不能为空，请重新执行并输入一个路径"
  case "$DATA_DIR" in
    /*) : ;;
    *) DATA_DIR="$SCRIPT_DIR/$DATA_DIR" ;;
  esac
  # 规范化：转绝对路径、合并 //、去掉 /./、存在的目录取真实路径
  case "$DATA_DIR" in
    /*) : ;;
    *)  DATA_DIR="$(pwd)/$DATA_DIR" ;;
  esac
  DATA_DIR="$(printf '%s' "$DATA_DIR" | sed -e 's#/\./#/#g' -e 's#//*#/#g')"
  DATA_DIR="${DATA_DIR%/}"
  if [ -d "$DATA_DIR" ]; then
    DATA_DIR="$(cd "$DATA_DIR" && pwd -P)"
  fi

  # 端口被其他程序占用时提醒（如果占用者就是我们将要删掉的容器，则没关系）
  if ! existing_container && port_in_use "$PORT"; then
    warn "端口 $PORT 已被其他程序占用，启动可能失败；可换一个端口重试"
  fi
  ok "端口：$PORT    数据目录：$DATA_DIR"
}

# ---------- 获取源码 ----------
fetch_source() {
  step "获取最新源码（Gitee ${GITEE_REPO}@${GITEE_BRANCH}）"
  if [ -z "$SOURCE_DIR" ]; then
    if [ -f "$SCRIPT_DIR/go.mod" ]; then
      SOURCE_DIR="$SCRIPT_DIR"
    else
      SOURCE_DIR="${HOME}/videodl-src"
    fi
  fi
  info "源码目录：$SOURCE_DIR"

  if [ -d "$SOURCE_DIR/.git" ] && has git; then
    info "检测到 git 仓库，尝试 git pull"
    if run git -C "$SOURCE_DIR" pull --ff-only; then
      ok "源码已更新"
      return 0
    fi
    warn "git pull 失败（本地有改动或网络问题），改为下载 zip"
  fi

  local url="https://gitee.com/${GITEE_REPO}/repository/archive/${GITEE_BRANCH}.zip"
  local tmp; tmp="$(mktemp -d)"
  # Gitee 对浏览器 UA 会返回登录页；curl 默认 UA 才能拿到 zip
  run curl -fsSL --retry 3 --connect-timeout 15 -o "$tmp/src.zip" "$url"
  # 校验：必须是 zip（前两个字节 PK）
  if [ "$DRY_RUN" != "1" ]; then
    local magic; magic="$(head -c 2 "$tmp/src.zip" 2>/dev/null || true)"
    [ "$magic" = "PK" ] || die "下载到的不是 zip（可能是 Gitee 要求登录/验证）。可手动下载 $url 后解压到 $SOURCE_DIR"
    has unzip || install_pkg unzip
    run unzip -q "$tmp/src.zip" -d "$tmp/x"
    local top; top="$(find "$tmp/x" -maxdepth 1 -mindepth 1 -type d | head -n1)"
    [ -n "$top" ] || die "解压后没有目录，源码包异常"
    # 目标目录不是 git 仓库且有旧内容：先整体挪到 .bak，避免旧文件残留导致编译失败
    if [ -d "$SOURCE_DIR" ] && [ -n "$(ls -A "$SOURCE_DIR" 2>/dev/null || true)" ] && [ ! -d "$SOURCE_DIR/.git" ]; then
      local bak="${SOURCE_DIR}.bak"
      warn "源码目录已有旧内容，先备份到 $bak（只保留最近一份）"
      [ -d "$bak" ] && run rm -rf "$bak"
      run mv "$SOURCE_DIR" "$bak"
    fi
    run mkdir -p "$SOURCE_DIR"
    run cp -a "$top/." "$SOURCE_DIR/"
  fi
  rm -rf "$tmp"
  ok "源码已就绪"
}

# ---------- 构建镜像 ----------
# registry_alive 判断镜像仓库是否可用（/v2/ 返回 200/401/403 都算活着）
registry_alive() {
  [ -z "$1" ] && return 1
  local code
  code="$(curl -s -o /dev/null -m 8 -w '%{http_code}' "https://$1/v2/" 2>/dev/null || true)"
  [ "$code" = "200" ] || [ "$code" = "401" ] || [ "$code" = "403" ]
}

# pick_images 按仓库拼出两个基础镜像名（空/docker.io = 官方简名）
pick_images() {
  local reg="$1"
  if [ -n "$reg" ] && [ "$reg" != "docker.io" ]; then
    GOLANG_IMG="${reg}/library/golang:1.26-alpine"
    ALPINE_IMG="${reg}/library/alpine:3.20"
  else
    GOLANG_IMG="golang:1.26-alpine"
    ALPINE_IMG="alpine:3.20"
  fi
}

build_image() {
  step "构建镜像"
  local ver="dev"
  if [ -f "$SOURCE_DIR/VERSION" ]; then ver="$(tr -d '\r\n' <"$SOURCE_DIR/VERSION")"; fi
  [ -f "$SOURCE_DIR/Dockerfile" ] || die "源码里没有 Dockerfile：$SOURCE_DIR"
  [ -f "$SOURCE_DIR/internal/web/static/index.html" ] || warn "源码里没有前端产物 internal/web/static（镜像内会缺页面）"

  if [ "$DRY_RUN" != "1" ] && docker image inspect "$IMAGE_NAME" >/dev/null 2>&1; then
    info "保留当前镜像为 ${IMAGE_PREV}（回滚用）"
    run docker tag "$IMAGE_NAME" "$IMAGE_PREV"
  fi

  info "版本：$ver（构建阶段会拉取 golang / alpine 基础镜像，首次较慢）"
  # 仓库探活：主仓库不可达就直接换备用，免得 build 卡在拉取阶段
  if [ -n "$REGISTRY" ] && ! registry_alive "$REGISTRY"; then
    warn "镜像仓库 $REGISTRY 不可达"
    if [ -n "$REGISTRY_FALLBACK" ] && [ "$REGISTRY_FALLBACK" != "$REGISTRY" ] && registry_alive "$REGISTRY_FALLBACK"; then
      REGISTRY="$REGISTRY_FALLBACK"
      info "已切换备用仓库：$REGISTRY"
    fi
  fi

  pick_images "$REGISTRY"
  info "基础镜像：${GOLANG_IMG}"
  info "Alpine 软件源：${APK_MIRROR:-官方}    Go 代理：${GOPROXY_ARG}"

  # 这里所有 build-arg 都显式传，不用数组（CentOS 7 的 bash 4.2 空数组展开会报错）
  if ! run docker build \
      --build-arg "APP_VERSION=$ver" \
      --build-arg "GOPROXY=$GOPROXY_ARG" \
      --build-arg "GOLANG_IMAGE=$GOLANG_IMG" \
      --build-arg "ALPINE_IMAGE=$ALPINE_IMG" \
      --build-arg "APK_MIRROR=$APK_MIRROR" \
      -t "$IMAGE_NAME" "$SOURCE_DIR"; then
    if [ -n "$REGISTRY_FALLBACK" ] && [ "$REGISTRY_FALLBACK" != "$REGISTRY" ]; then
      warn "使用 $REGISTRY 构建失败，改用备用仓库 $REGISTRY_FALLBACK 重试"
      REGISTRY="$REGISTRY_FALLBACK"
      pick_images "$REGISTRY"
      info "基础镜像：${GOLANG_IMG}"
      run docker build \
        --build-arg "APP_VERSION=$ver" \
        --build-arg "GOPROXY=$GOPROXY_ARG" \
        --build-arg "GOLANG_IMAGE=$GOLANG_IMG" \
        --build-arg "ALPINE_IMAGE=$ALPINE_IMG" \
        --build-arg "APK_MIRROR=$APK_MIRROR" \
        -t "$IMAGE_NAME" "$SOURCE_DIR" || die "主仓库和备用仓库都构建失败，请检查网络或用 --official"
    else
      die "镜像构建失败（可加 --registry 换仓库，或 --official 用官方源）"
    fi
  fi
  ok "镜像构建完成：$IMAGE_NAME"
}

# ---------- 启动 / 更新容器 ----------
start_container() {
  local image="$1"
  run docker run -d \
    --name "$CONTAINER_NAME" \
    --restart unless-stopped \
    -p "${PORT}:5200" \
    -v "${DATA_DIR}:/app/videodata" \
    -e "TZ=${TZ:-Asia/Shanghai}" \
    -e "VDL_CONFIG=/app/videodata/config.yml" \
    --init \
    --stop-timeout 30 \
    "$image"
}

run_container() {
  step "启动容器"
  run mkdir -p "$DATA_DIR"
  if [ "$DRY_RUN" != "1" ]; then
    [ -d "$DATA_DIR" ] || die "数据目录无法创建：$DATA_DIR"
  fi

  if existing_container; then
    info "停止并删除旧容器 ${CONTAINER_NAME}"
    run docker rm -f "$CONTAINER_NAME"
  fi

  start_container "$IMAGE_NAME" || {
    warn "新镜像启动失败"
    if [ "$DRY_RUN" != "1" ] && docker image inspect "$IMAGE_PREV" >/dev/null 2>&1; then
      warn "回滚到上一个镜像 ${IMAGE_PREV}"
      start_container "$IMAGE_PREV" || die "回滚也失败了，请手动检查：docker logs $CONTAINER_NAME"
    else
      die "启动失败，请检查：docker logs $CONTAINER_NAME"
    fi
  }
  ok "容器已启动"
}

wait_healthy() {
  [ "$DRY_RUN" = "1" ] && return 0
  step "等待服务就绪"
  local i=0 tries="${HEALTH_RETRIES:-40}"
  while [ "$i" -lt "$tries" ]; do
    i=$((i + 1))
    if has curl && curl -fsS --max-time 3 "http://127.0.0.1:${PORT}/healthz" >/dev/null 2>&1; then
      ok "服务已就绪"
      return 0
    fi
    if ! has curl && docker exec "$CONTAINER_NAME" wget -qO- http://127.0.0.1:5200/healthz >/dev/null 2>&1; then
      ok "服务已就绪（容器内 wget 探活）"
      return 0
    fi
    # 容器已退出就别再等
    if ! docker ps --format '{{.Names}}' | grep -qx "$CONTAINER_NAME"; then
      warn "容器已退出，最近日志："
      docker logs --tail 30 "$CONTAINER_NAME" || true
      die "启动失败，请检查上面的日志"
    fi
    sleep 1
  done
  warn "${tries} 秒内未通过健康检查，服务可能仍在启动；查看日志：docker logs -f $CONTAINER_NAME"
}

summary() {
  local ip=""
  if has hostname; then
    ip="$(hostname -I 2>/dev/null | awk '{print $1}' || true)"
  fi
  printf '\n%s部署完成%s\n' "$C_GRN" "$C_OFF"
  printf '  访问地址：http://%s:%s\n' "${ip:-服务器IP}" "$PORT"
  printf '  数据目录：%s\n' "$DATA_DIR"
  printf '  配置文件：%s/config.yml（首次启动自动生成，改完重启容器生效）\n' "$DATA_DIR"
  printf '  容器名称：%s    镜像：%s\n' "$CONTAINER_NAME" "$IMAGE_NAME"
  printf '  基础镜像：%s（golang:1.26-alpine / alpine:3.20）\n' "${REGISTRY:-官方 Docker Hub}"
  printf '\n常用命令：\n'
  printf '  docker logs -f %s          # 看日志\n' "$CONTAINER_NAME"
  printf '  docker restart %s          # 重启\n' "$CONTAINER_NAME"
  printf '  sudo %s --yes             # 再次执行 = 更新到最新版本\n' "$0"
  printf '  docker tag %s %s && docker rm -f %s && docker run ...   # 回滚用旧镜像\n' "$IMAGE_PREV" "$IMAGE_NAME" "$CONTAINER_NAME"
}

# ---------- 主流程 ----------
main() {
  check_platform
  need_root "$@"
  detect_os

  if [ "$NO_INSTALL" = "1" ]; then
    has docker || die "--no-install 指定了，但系统没有 docker"
    docker_ready || die "docker 命令存在但服务不可用，请先启动 Docker"
  elif docker_ready; then
    info "Docker 已安装：$(docker --version 2>/dev/null | head -n1)"
  else
    install_docker
  fi
  configure_registry_mirror

  # 健康检查要用 curl；极简系统可能没有
  if ! has curl; then
    warn "缺少 curl，尝试安装（健康检查与下载源码都要用）"
    install_pkg curl
  fi

  # 需要在有 docker 之后判断已有容器
  resolve_settings
  fetch_source
  build_image
  run_container
  wait_healthy
  summary
}

main "$@"
