#!/usr/bin/env bash
# ============================================================
# 影视库（videodl）Linux 一键部署 / 更新脚本（国内链路优化版）
#
# 做五件事：
#   1. 没有 Docker 就自动安装（优先阿里云 docker-ce 源，国内不卡）；
#   2. 从 Gitee 拉最新源码（有 git 用 git clone/pull，没有则下 zip），构建 docker 镜像；
#   3. 全程走国内链路：国内镜像站拉基础镜像 + 国内 Go 模块代理 + 国内 Alpine 源；
#   4. 询问（或按参数使用）宿主端口和数据存储路径，启动容器；
#   5. 已经有 videodl 容器时，重建镜像并用原配置重新创建容器（= 更新）。
#
# 用法：
#   sudo ./deploy.sh                     # 交互式：问端口、数据目录
#   sudo ./deploy.sh --yes               # 全部用默认值 / 沿用已有容器配置
#   sudo ./deploy.sh --port 8080 --data /data/videodl
#   ./deploy.sh --check                  # 只做网络体检（不装、不构建）
#   ./deploy.sh --dry-run                # 只打印将要执行的命令，不安装、不构建
#   ./deploy.sh --help
#
# 说明：容器内配置在 <数据目录>/config.yml，首次启动自动生成；
#       更新只会重建镜像和容器，数据目录（含视频、数据库、配置）不受影响；
#       旧容器里手动加过的 VDL_* 变量（更新令牌、管理口令等）会自动带到新容器。
#
# 环境变量（等价于同名参数）：
#   IMAGE_NAME IMAGE_PREV CONTAINER_NAME GITEE_REPO GITEE_BRANCH DEFAULT_PORT DEFAULT_DATA
#   GOPROXY_ARG GOSUMDB_ARG GOTOOLCHAIN_ARG APK_MIRROR APK_FALLBACK_MIRRORS
#   REGISTRY REGISTRY_FALLBACK REGISTRY_CANDIDATES DOCKER_CE_MIRROR
#   BASE_IMAGE_TAG TARGET_ARCH_ARG TARGET_OS_ARG BUILD_NETWORK PULL_TIMEOUT HEALTH_RETRIES
#   注意：Go 代理叫 GOPROXY_ARG（**故意**不叫 GOPROXY），避免和宿主机已导出的
#        Go 环境变量撞名；`GOPROXY=... sudo ./deploy.sh` 是无效的，请用 GOPROXY_ARG。
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

# Go 模块代理：竖线分隔 = 前一个代理**出错**（超时/DNS 抖动）也立刻换下一个。
# 国内默认串了 4 个代理 + direct 兜底，任何一个抽风都不会让构建卡死。
# 海外机器用 --official（换成 proxy.golang.org）。
GOPROXY_ARG="${GOPROXY_ARG:-https://goproxy.cn|https://mirrors.aliyun.com/goproxy/|https://mirrors.cloud.tencent.com/go/|https://goproxy.io|direct}"
# 校验数据库：**不要关**（国内也用得上）。
# sum.golang.org 本身国内连不上，但 Go 会把 sumdb 查询**转发给 GOPROXY 里的代理**
# （<代理>/sumdb/sum.golang.org/lookup/...），实测 goproxy.cn / 阿里云 / 腾讯云都返回 200。
# 关掉（GOSUMDB=off）会让 golang.org/toolchain 无法通过校验 —— 基础镜像 Go 版本低于
# go.mod 要求时，go 自动下载工具链会直接报
# "checksum database disabled by GOSUMDB=off"，整个构建挂掉。
GOSUMDB_ARG="${GOSUMDB_ARG:-sum.golang.org}"
# Go 工具链策略：auto = 基础镜像自带的 Go 低于 go.mod 要求时，自动经 GOPROXY 下载
# 对应版本的工具链（国内代理实测可下，约 70MB，一次性开销）；
# local = 不下载，版本不够立刻失败并给出可操作提示（离线环境用）。
GOTOOLCHAIN_ARG="${GOTOOLCHAIN_ARG:-auto}"

# Alpine 软件源：默认阿里云，避免 apk add 卡在 dl-cdn.alpinelinux.org；
# 阿里云抽风时依次试清华源。
APK_MIRROR="${APK_MIRROR:-https://mirrors.aliyun.com/alpine}"
APK_FALLBACK_MIRRORS="${APK_FALLBACK_MIRRORS:-https://mirrors.tuna.tsinghua.edu.cn/alpine}"

# 基础镜像仓库：默认 1panel（实测大层 ~1.2MB/s）。1ms.run 的 manifest 很快但大层走
# CloudFront，实测只有 ~29KB/s，所以只作自动兜底。脚本会按下面的候选顺序逐个探活，
# 第一个可用（manifest 能取到）的就用它；构建失败还会自动换下一个重试。
REGISTRY="${REGISTRY:-docker.1panel.live}"
REGISTRY_FALLBACK="${REGISTRY_FALLBACK:-docker.1ms.run}"
# ⚠️ 候选里**不要放** docker.xuanyuan.me / docker.nju.edu.cn：实测返回裸 403
#    （响应里没有 WWW-Authenticate 头），Docker 拿不到 token，镜像站实际不可用。
#    探活逻辑已改成「200 或 401+WWW-Authenticate 才算可用」，所以即使误加也会被跳过，
#    只是白白多等一轮探测。
REGISTRY_CANDIDATES="${REGISTRY_CANDIDATES:-docker.1panel.live docker.1ms.run docker.m.daocloud.io docker.amingg.com}"
# Docker 本体安装源（apt / dnf 用）：国内默认阿里云 docker-ce
DOCKER_CE_MIRROR="${DOCKER_CE_MIRROR:-https://mirrors.aliyun.com/docker-ce}"
# 基础镜像 tag：go.mod 要求 go 1.26，别低于这个版本
BASE_IMAGE_TAG="${BASE_IMAGE_TAG:-1.26-alpine}"
# 目标架构：留空 = 自动（由 Docker/BuildKit 决定，跟随宿主或 --platform）。
#   - BuildKit 会自动注入 TARGETARCH（对应 --platform），脚本**不要**再传一遍，
#     否则会盖掉 --platform 的值；
#   - 老 Docker（20.10 classic builder）没有 TARGETARCH，Dockerfile 会回落到
#     `go env GOARCH`（= 宿主架构），行为同样正确。
#   - 显式指定只在「要给别的架构出包」时用（需要 QEMU 模拟）。
TARGET_ARCH_ARG="${TARGET_ARCH_ARG:-}"
TARGET_OS_ARG="${TARGET_OS_ARG:-linux}"
# docker build 的网络模式：留空用默认；有些 NAS 的 build 网络 DNS 抖，可设 host
BUILD_NETWORK="${BUILD_NETWORK:-}"
# 拉基础镜像的超时（秒）
PULL_TIMEOUT="${PULL_TIMEOUT:-900}"

PORT=""
DATA_DIR=""
ASSUME_YES=0
DRY_RUN=0
NO_INSTALL=0
REGISTRY_MIRROR=""
FORCE=0
CHECK_ONLY=0
BUILD_PULL=0
BUILD_NO_CACHE=0
# 重建容器时用来「带过去」旧容器 VDL_* 变量的临时 env 文件（空 = 还没生成）
CARRY_ENV=""

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
影视库（videodl）Linux 一键部署 / 更新脚本（国内链路优化版）

用法：
  sudo ./deploy.sh                     交互式：询问端口与数据目录
  sudo ./deploy.sh --yes               全部用默认值；已有容器则沿用其配置
  sudo ./deploy.sh --port 8080 --data /data/videodl
  sudo ./deploy.sh --check             只做网络体检（不安装、不构建、不启动）
  ./deploy.sh --dry-run                只打印将执行的命令（不安装、不构建、不启动）

参数：
  --port N              宿主机端口（容器内固定 5200）
  --data PATH           数据存储路径（数据库、视频、config.yml 都在这里）
  --source-dir PATH     源码目录（默认脚本目录；没有源码则放 ~/videodl-src）
  --registry HOST       基础镜像仓库（默认 docker.1panel.live；传 docker.io 用官方）
  --registry-fallback H 备用仓库（默认 docker.1ms.run，主仓库不可达/构建失败时自动切换）
  --registry-list "H1 H2 ..." 候选镜像站列表（按顺序探活，第一个可用就用它）
  --apk-mirror URL      Alpine 软件源（默认 https://mirrors.aliyun.com/alpine）
  --apk-fallback URL    Alpine 备用源（默认 https://mirrors.tuna.tsinghua.edu.cn/alpine）
  --goproxy URL         Go 模块代理（默认 goproxy.cn|阿里云|腾讯云|goproxy.io|direct，竖线=出错也换下一个）
  --gosumdb NAME        校验数据库（默认 sum.golang.org；查询经 GOPROXY 转发，国内可用。
                        不要设 off：关掉会导致工具链下载报 "checksum database disabled"）
  --gotoolchain VALUE   Go 工具链策略（默认 auto=版本不够时自动经代理下载工具链；
                        local=不下载，版本不够立即失败并提示）
  --arch ARCH           目标架构（默认自动；只在给别的架构出包时用，如 arm64，
                        需要宿主机装 QEMU）
  --docker-ce-mirror U  Docker 本体安装源（默认 https://mirrors.aliyun.com/docker-ce）
  --base-image-tag TAG  基础镜像 tag（默认 1.26-alpine，需 >= go.mod 的 go 版本）
  --build-network NET   docker build 的网络模式（如 host，默认不指定）
  --timeout SEC         拉基础镜像的超时秒数（默认 900）
  --pull                构建前强制刷新基础镜像（docker build --pull）
  --no-cache            构建时不用缓存（docker build --no-cache）
  --official            全部切回官方源（海外机器用：Docker Hub + Alpine CDN + proxy.golang.org）
  --registry-mirror URL 额外给 Docker daemon 配镜像加速（会重启 docker，默认不改；
                        可给多个，用逗号分隔；会保留 daemon.json 里已有的其它配置）
  --yes, -y             不交互，使用默认值 / 沿用已有容器
  --no-install          不自动安装 Docker（已装好时用）
  --check               只体检网络与镜像源可达性
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
    --port)              PORT="${2:-}"; shift 2 ;;
    --data)              DATA_DIR="${2:-}"; shift 2 ;;
    --source-dir)        SOURCE_DIR="${2:-}"; shift 2 ;;
    --registry-mirror)   REGISTRY_MIRROR="${2:-}"; shift 2 ;;
    --registry)          REGISTRY="${2:-}"; shift 2 ;;
    --registry-fallback) REGISTRY_FALLBACK="${2:-}"; shift 2 ;;
    --registry-list)     REGISTRY_CANDIDATES="${2:-}"; shift 2 ;;
    --apk-mirror)        APK_MIRROR="${2:-}"; shift 2 ;;
    --apk-fallback)      APK_FALLBACK_MIRRORS="${2:-}"; shift 2 ;;
    --goproxy)           GOPROXY_ARG="${2:-}"; shift 2 ;;
    --gosumdb)           GOSUMDB_ARG="${2:-}"; shift 2 ;;
    --gotoolchain)       GOTOOLCHAIN_ARG="${2:-}"; shift 2 ;;
    --arch)              TARGET_ARCH_ARG="${2:-}"; shift 2 ;;
    --docker-ce-mirror)  DOCKER_CE_MIRROR="${2:-}"; shift 2 ;;
    --base-image-tag)    BASE_IMAGE_TAG="${2:-}"; shift 2 ;;
    --build-network)     BUILD_NETWORK="${2:-}"; shift 2 ;;
    --timeout)           PULL_TIMEOUT="${2:-}"; shift 2 ;;
    --pull)              BUILD_PULL=1; shift ;;
    --no-cache)          BUILD_NO_CACHE=1; shift ;;
    --check)             CHECK_ONLY=1; shift ;;
    --official)
      REGISTRY="docker.io"; REGISTRY_FALLBACK=""; REGISTRY_CANDIDATES="docker.io"
      APK_MIRROR=""; APK_FALLBACK_MIRRORS=""
      GOPROXY_ARG="https://proxy.golang.org,direct"; GOSUMDB_ARG="sum.golang.org"
      GOTOOLCHAIN_ARG="auto"
      DOCKER_CE_MIRROR=""; shift ;;
    --yes|-y)            ASSUME_YES=1; shift ;;
    --dry-run)           DRY_RUN=1; shift ;;
    --no-install)        NO_INSTALL=1; shift ;;
    --force)             FORCE=1; shift ;;
    -h|--help)           usage ;;
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
if [ "$PIPED" = "1" ] && [ "$ASSUME_YES" != "1" ] && [ "$CHECK_ONLY" != "1" ]; then
  die "从管道执行时无法交互输入端口和数据目录。请先下载脚本再执行：
  curl -fsSL https://gitee.com/${GITEE_REPO}/raw/${GITEE_BRANCH}/deploy.sh -o deploy.sh
  chmod +x deploy.sh && sudo ./deploy.sh
或者显式指定：sudo bash -s -- --yes --port 5200 --data /opt/videodata"
fi

need_root() {
  [ "$DRY_RUN" = "1" ] && return 0
  [ "$CHECK_ONLY" = "1" ] && return 0
  if [ "$FORCE" = "1" ]; then
    warn "已用 --force 跳过 root 检查（仅供调试）"
    return 0
  fi
  if [ "$(id -u)" -ne 0 ]; then
    die "请用 root 执行：sudo $0 ${ORIG_ARGS}"
  fi
}

# 在 Windows/Git Bash 上只允许 --dry-run / --check，方便先看流程
check_platform() {
  case "$(uname -s)" in
    Linux) : ;;
    *)
      if [ "$DRY_RUN" = "1" ] || [ "$CHECK_ONLY" = "1" ]; then
        warn "当前不是 Linux（$(uname -s)），仅以演练/体检方式运行"
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

# 带超时的执行（系统没有 timeout 时直接执行）
run_timeout() {
  local secs="$1"; shift
  if has timeout; then timeout "$secs" "$@"; else "$@"; fi
}

# ---------- 系统识别 ----------
OS_ID=""; OS_LIKE=""; OS_CODENAME=""; PKG=""
detect_os() {
  if [ -r /etc/os-release ]; then
    # shellcheck disable=SC1091
    . /etc/os-release
    OS_ID="${ID:-}"; OS_LIKE="${ID_LIKE:-}"; OS_CODENAME="${VERSION_CODENAME:-}"
  fi
  if [ -z "$OS_CODENAME" ]; then
    OS_CODENAME="$(lsb_release -cs 2>/dev/null || true)"
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
  info "系统：${PRETTY_NAME:-$(uname -s)}（包管理器：${PKG:-未知}${OS_CODENAME:+ / $OS_CODENAME}）"
}

# Debian 系架构名（amd64 / arm64）
deb_arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo amd64 ;;
    aarch64|arm64) echo arm64 ;;
    armv7l|armv7) echo armhf ;;
    *) echo "$(uname -m)" ;;
  esac
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

# 用国内 docker-ce 源安装 Docker（apt / dnf / yum）。
# get.docker.com 在国内经常超时或极慢，所以优先走 mirrors.aliyun.com/docker-ce。
install_docker_from_cn_mirror() {
  [ -n "$DOCKER_CE_MIRROR" ] || return 1
  if [ "$DRY_RUN" = "1" ]; then
    info "演练：从国内源安装 Docker（$DOCKER_CE_MIRROR）"
    return 0
  fi
  case "$PKG" in
    apt)
      install_pkg ca-certificates curl gnupg || return 1
      local distro="" codename="$OS_CODENAME" arch
      case " $OS_ID $OS_LIKE " in
        *ubuntu*)  distro=ubuntu ;;
        *debian*)  distro=debian ;;
        *raspbian*) distro=debian ;;
      esac
      [ -n "$distro" ] || { warn "不认识的 Debian 系发行版：$OS_ID"; return 1; }
      [ -n "$codename" ] || { warn "取不到发行版代号（$codename 为空）"; return 1; }
      arch="$(deb_arch)"
      info "配置阿里云 docker-ce 源：$distro/$codename ($arch)"
      run mkdir -p /etc/apt/keyrings
      if ! curl -fsSL --max-time 30 "${DOCKER_CE_MIRROR}/linux/${distro}/gpg" -o /etc/apt/keyrings/docker.asc; then
        warn "下载 GPG key 失败"; return 1
      fi
      run chmod a+r /etc/apt/keyrings/docker.asc
      printf 'deb [arch=%s signed-by=/etc/apt/keyrings/docker.asc] %s/linux/%s %s stable\n' \
        "$arch" "$DOCKER_CE_MIRROR" "$distro" "$codename" >/etc/apt/sources.list.d/docker.list
      # 下面两步任何一步失败都要把国内源撤掉，否则后面的 apt-get update 会被坏源带崩
      if ! run env DEBIAN_FRONTEND=noninteractive apt-get update -qq; then
        warn "apt-get update 失败（国内源可能不含 $codename），移除该源后回退"
        rm -f /etc/apt/sources.list.d/docker.list
        return 1
      fi
      if ! run env DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin; then
        warn "安装 docker-ce 失败，移除该源后回退"
        rm -f /etc/apt/sources.list.d/docker.list
        return 1
      fi
      ;;
    dnf|yum)
      install_pkg curl || return 1
      local repo="${DOCKER_CE_MIRROR}/linux/centos/docker-ce.repo"
      info "配置阿里云 docker-ce 源：$repo"
      if has dnf; then
        run dnf config-manager --add-repo "$repo" || return 1
      elif has yum-config-manager; then
        run yum-config-manager --add-repo "$repo" || return 1
      else
        warn "没有 dnf / yum-config-manager"; return 1
      fi
      run ${PKG} install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
      ;;
    *) return 1 ;;
  esac
  return 0
}

# 官方安装脚本（海外/国内都可能被墙，慢）
install_docker_from_official_script() {
  if [ "$DRY_RUN" = "1" ]; then
    run sh -c 'curl -fsSL https://get.docker.com | sh'
    return 0
  fi
  curl -fsSL --max-time 20 https://get.docker.com -o /tmp/get-docker.sh || return 1
  run sh /tmp/get-docker.sh
}

install_docker() {
  step "没有可用的 Docker，开始安装"
  if [ "$DRY_RUN" = "1" ]; then
    if [ -n "$DOCKER_CE_MIRROR" ]; then
      info "演练：国内源 $DOCKER_CE_MIRROR（apt/dnf 装 docker-ce + buildx + compose 插件）"
    else
      info "演练：官方脚本 https://get.docker.com"
    fi
    run sh -c 'echo "安装 docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin"'
    return 0
  fi

  # 1) 国内 docker-ce 源（最快最稳）
  if install_docker_from_cn_mirror; then
    start_docker_service
    sleep 2
    if docker_ready; then
      ok "已通过国内源安装 Docker（${DOCKER_CE_MIRROR:-内置镜像}）"
      return 0
    fi
    warn "Docker 已装好但服务没起来，继续走官方脚本兜底"
  fi
  warn "国内 docker-ce 源安装失败，改用官方安装脚本"

  # 2) 官方安装脚本
  case "$PKG" in
    apt)
      install_pkg ca-certificates curl gnupg
      if install_docker_from_official_script; then
        start_docker_service
        sleep 3
        if docker_ready; then
          ok "已通过官方脚本安装 Docker"
          return 0
        fi
        warn "官方脚本执行完了但 docker 还没就绪"
      fi
      warn "官方脚本失败，改用发行版软件包 docker.io"
      install_pkg docker.io
      ;;
    dnf|yum)
      install_pkg curl
      if install_docker_from_official_script; then
        start_docker_service
        sleep 3
        if docker_ready; then
          ok "已通过官方脚本安装 Docker"
          return 0
        fi
        warn "官方脚本执行完了但 docker 还没就绪"
      fi
      warn "官方脚本失败，改用发行版软件包"
      install_pkg docker
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

# ---------- Docker daemon 镜像加速（可选） ----------
# 注意：改 daemon.json 会重启 docker，影响机器上其它容器，所以默认不开。
# 这里只改 registry-mirrors 一个键，daemon.json 里其它配置（data-root、log-opts 等）原样保留。
write_registry_mirrors() {
  # $1 = 镜像地址列表（逗号或空格分隔）
  local mirrors f=/etc/docker/daemon.json tmp body
  mirrors="$(printf '%s' "$1" | tr ', ' '\n\n' | sed '/^[[:space:]]*$/d' | \
    awk 'BEGIN{first=1}{ if(first){printf "%s",$0; first=0} else printf ",%s",$0 } END{ if(!first) print "" }')"
  [ -n "$mirrors" ] || { warn "没有可用的镜像加速地址"; return 1; }

  run mkdir -p /etc/docker
  if [ "$DRY_RUN" = "1" ]; then
    printf '%s  + 写入 %s: {"registry-mirrors":[%s]}%s\n' "$C_YEL" "$f" "$mirrors" "$C_OFF"
    return 0
  fi

  [ -f "$f" ] && cp -a "$f" "$f.bak.$(date +%s)"
  tmp="$(mktemp)"
  if [ -f "$f" ] && [ -s "$f" ]; then
    if [ "$(wc -l <"$f")" -le 1 ]; then
      # 单行 JSON：直接去掉最外层大括号
      body="$(sed -e 's/^[[:space:]]*{[[:space:]]*//' -e 's/[[:space:]]*}[[:space:]]*$//' "$f" | sed '/^[[:space:]]*$/d')"
    else
      # 多行 JSON：
      #   1) 去掉原有的 registry-mirrors 段（单行数组整行丢；多行数组连同到 ] 为止一起丢）
      #   2) 去掉最外层大括号，取出对象内部内容
      body="$(awk '
        /"registry-mirrors"[ \t]*:/ {
          if (index($0, "]") > 0) next
          skip = 1; next
        }
        skip == 1 { if (index($0, "]") > 0) skip = 0; next }
        { print }
      ' "$f" | sed -e '1{/^[[:space:]]*{[[:space:]]*$/d}' -e '${/^[[:space:]]*}[[:space:]]*$/d}' \
        | sed '/^[[:space:]]*$/d')"
    fi
    # 内部内容最后一行不能带逗号（否则拼出来的 JSON 非法）
    body="$(printf '%s\n' "$body" | sed -e '$ s/[[:space:]]*,[[:space:]]*$//')"
  else
    body=""
  fi

  {
    printf '{\n'
    printf '  "registry-mirrors": ["%s"]' "$(printf '%s' "$mirrors" | sed 's/,/", "/g')"
    if [ -n "$body" ]; then printf ',\n%s\n' "$body"; else printf '\n'; fi
    printf '}\n'
  } >"$tmp"

  # 校验：有 python3 / jq 就用它们校验；都没有时做括号 + 引号配平的粗检
  if ! python3 -c 'import json,sys; json.load(open(sys.argv[1]))' "$tmp" 2>/dev/null; then
    if ! jq -e . "$tmp" >/dev/null 2>&1; then
      local bo bc co cc
      bo="$(tr -cd '{' <"$tmp" | wc -c)"; bc="$(tr -cd '}' <"$tmp" | wc -c)"
      co="$(tr -cd '"' <"$tmp" | wc -c)"
      if [ "$bo" != "$bc" ] || [ $((co % 2)) -ne 0 ]; then
        warn "生成的 daemon.json 看起来不合法，未覆盖原文件：$f（原文件已备份）"
        rm -f "$tmp"
        return 1
      fi
    fi
  fi
  mv "$tmp" "$f"
  ok "已写入镜像加速：$f"
  if has systemctl; then run systemctl restart docker; else run service docker restart; fi
  sleep 3
  docker_ready || warn "docker 重启后暂时不可用，请检查：systemctl status docker"
}

configure_registry_mirror() {
  # 国内拉不到镜像时用：把镜像加速写进 daemon.json
  [ -n "$REGISTRY_MIRROR" ] || return 0
  step "配置 Docker 镜像加速：$REGISTRY_MIRROR"
  write_registry_mirrors "$REGISTRY_MIRROR"
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

# 把旧容器里用户自己加的 VDL_* 环境变量写成 docker --env-file 格式（一行一个 VAR=VALUE）。
# 为什么需要：脚本重建容器是 `docker rm -f` + `docker run`，只有在脚本里写死的 -e 会保留，
# 用户当初手动加的（例如 VDL_UPDATE_TOKEN、VDL_ADMIN_TOKEN、VDL_PUBLIC_URL）会静静丢掉，
# 表现为「更新一次之后管理口令/更新令牌失效」。
# VDL_CONFIG 不在这里带：它由脚本自己设，统一指向 volume 上的 config.yml。
container_env_file() {
  local f
  f="$(mktemp)"
  if existing_container; then
    docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$CONTAINER_NAME" 2>/dev/null \
      | grep -E '^VDL_[A-Za-z0-9_]+=' | grep -v '^VDL_CONFIG=' >>"$f" 2>/dev/null || true
  fi
  printf '%s' "$f"
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

# ---------- 网络体检 ----------
probe_code() {
  # probe_code <url> [timeout] -> 打印 HTTP 状态码（失败为 000）
  local url="$1" t="${2:-8}" code
  code="$(curl -s -o /dev/null -m "$t" -w '%{http_code}' "$url" 2>/dev/null)" || code="000"
  printf '%s' "$code"
}

check_network() {
  step "网络体检（国内链路）"
  local url code
  printf '  %-46s %s\n' "Gitee 源码仓库" "https://gitee.com/${GITEE_REPO}"
  url="https://gitee.com/${GITEE_REPO}"
  code="$(probe_code "$url")"
  [ "$code" = "000" ] && warn "    Gitee 不可达（$code），源码拉取会失败" || ok "    可达（HTTP $code）"

  # Go 模块代理（取竖线分隔的第一个）
  local gop="${GOPROXY_ARG%%|*}"
  url="${gop%%,*}"
  printf '  %-46s %s\n' "Go 模块代理" "$url"
  code="$(probe_code "${url}/golang.org/x/text/@v/list")"
  [ "$code" = "000" ] || [ "$code" = "404" ] && warn "    代理不可用（HTTP $code）" || ok "    可用（HTTP $code）"

  # 校验数据库查询：Go 会把 sumdb 查询转发给 GOPROXY 里的代理
  # （<代理>/sumdb/sum.golang.org/lookup/...）。这个不通的话，
  # 基础镜像 Go 版本不够时自动补齐工具链会失败（checksum database 校验不过）。
  printf '  %-46s %s\n' "校验数据库转发（经 $url）" "sum.golang.org/lookup"
  code="$(probe_code "${url}/sumdb/sum.golang.org/lookup/golang.org/x/text@v0.3.0" 12)"
  if [ "$code" = "200" ]; then
    ok "    可用（HTTP $code），工具链可自动补齐"
  elif [ "$code" = "000" ]; then
    warn "    不可达（HTTP $code）—— 若基础镜像 Go 版本低于 go.mod 要求，构建会失败"
  else
    warn "    返回 HTTP $code（可能该查询不存在，不一定是故障）"
  fi

  printf '  %-46s %s\n' "Alpine 软件源" "${APK_MIRROR:-官方 dl-cdn}"
  if [ -n "$APK_MIRROR" ]; then
    code="$(probe_code "${APK_MIRROR}/latest-stable/main/x86_64/APKINDEX.tar.gz" 12)"
    [ "$code" = "200" ] && ok "    可用（HTTP $code）" || warn "    可能不可用（HTTP $code）"
  fi

  printf '  %-46s %s\n' "Docker Hub 直连" "https://registry-1.docker.io/v2/"
  code="$(probe_code "https://registry-1.docker.io/v2/")"
  if [ "$code" = "000" ]; then
    warn "    直连不通（正常，国内需走镜像站）"
  else
    ok "    可达（HTTP $code）"
  fi

  step "基础镜像站探活（按顺序取第一个可用的）"
  local c found=""
  for c in $(candidate_registries); do
    printf '  %-46s ' "${c}/library/golang:${BASE_IMAGE_TAG}"
    if registry_image_alive "$c"; then
      ok "可用"
      [ -z "$found" ] && found="$c"
    else
      warn "不可用"
    fi
  done
  if [ -n "$found" ]; then
    ok "将使用镜像站：$found"
  else
    warn "所有镜像站都探活失败，构建时可能会卡在拉取阶段（可换 --registry 或加 --official）"
  fi
}

# Docker / BuildKit 状态体检：老版本 Docker（20.10 classic builder）与新版本行为不同，
# 这里把关键结论打出来，省得构建失败时不知道是环境问题还是脚本问题。
check_docker_env() {
  step "Docker 环境体检"
  if ! has docker; then
    warn "    没有 docker 命令（体检阶段可忽略，正式部署时会自动安装）"
    return 0
  fi
  info "    版本：$(docker --version 2>/dev/null | head -n1)"
  info "    宿主架构：$(uname -m)（docker.1panel.live 等镜像站会按这个架构给 manifest）"

  # BuildKit 可用性：决定 TARGETARCH 是自动注入还是走 Dockerfile 内的回落逻辑。
  local bk="classic builder（老式构建器）"
  if docker buildx version >/dev/null 2>&1; then
    bk="BuildKit（$(docker buildx version 2>/dev/null | head -n1)）"
  elif docker info --format '{{json .DriverStatus}}' 2>/dev/null | grep -qi buildkit; then
    bk="BuildKit（内置）"
  fi
  info "    构建器：$bk"
  case "$bk" in
    BuildKit*)
      info "    目标架构 TARGETARCH 由构建器自动注入，脚本不会覆盖它" ;;
    *)
      info "    目标架构由 Dockerfile 内回落（go env GOARCH = 宿主架构），无需额外设置" ;;
  esac

  # 镜像加速配置（国内直连 Docker Hub 基本连不上）
  local mr
  mr="$(docker info --format '{{range .RegistryConfig.Mirrors}}{{.}}{{"\n"}}{{end}}' 2>/dev/null | sed '/^[[:space:]]*$/d' | tr '\n' ' ')"
  if [ -n "$mr" ]; then
    ok "    已配置镜像加速：$mr"
  else
    warn "    未配置镜像加速（不影响本脚本：基础镜像走 REGISTRY_CANDIDATES 里的国内镜像站）"
  fi
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

  # 1) 已经是 git 仓库 → pull（最快）
  if [ -d "$SOURCE_DIR/.git" ] && has git; then
    info "检测到 git 仓库，尝试 git pull"
    if run git -C "$SOURCE_DIR" pull --ff-only; then
      ok "源码已更新（git pull）"
      return 0
    fi
    warn "git pull 失败（本地有改动或网络问题），改为重新下载"
  fi

  # 2) 空目录 + 有 git → 浅克隆（比下 zip 小很多）
  if has git && { [ ! -d "$SOURCE_DIR" ] || [ -z "$(ls -A "$SOURCE_DIR" 2>/dev/null || true)" ]; }; then
    info "尝试 git 浅克隆（--depth 1）"
    local tmp; tmp="$(mktemp -d)"
    if run git clone --depth 1 --branch "$GITEE_BRANCH" "https://gitee.com/${GITEE_REPO}.git" "$tmp/src"; then
      run mkdir -p "$SOURCE_DIR"
      run cp -a "$tmp/src/." "$SOURCE_DIR/"
      rm -rf "$tmp"
      ok "源码已就绪（git clone）"
      return 0
    fi
    rm -rf "$tmp"
    warn "git 克隆失败，改用下载 zip"
  fi

  # 3) 下载 zip（没有 git 或克隆失败时）
  local url="https://gitee.com/${GITEE_REPO}/repository/archive/${GITEE_BRANCH}.zip"
  local tmp; tmp="$(mktemp -d)"
  info "下载源码包：$url"
  # ⚠️ 必须用命令行风格 UA：Gitee 对**浏览器 UA** 会返回「下载仓库」登录/验证页
  #    （实测浏览器 UA 拿到的是 46KB 的 HTML，不是 zip）。
  #    固定 UA 与容器内 updater 保持一致（internal/updater/updater.go 的 cliUserAgent）。
  if ! run curl -fsSL --retry 3 --retry-delay 3 --connect-timeout 15 \
        -A 'curl/8.10.1' -o "$tmp/src.zip" "$url"; then
    rm -rf "$tmp"
    die "下载源码失败：$url（检查网络，或手动下载后解压到 $SOURCE_DIR）"
  fi
  # 校验：必须是 zip（前两个字节 PK）—— 防止把登录页 HTML 当源码包
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

# ---------- 镜像仓库探活与选择 ----------
# candidate_registries：按「用户指定 → 备用 → 候选列表」去重后输出候选镜像站
candidate_registries() {
  printf '%s\n' "$REGISTRY" "$REGISTRY_FALLBACK" $REGISTRY_CANDIDATES | \
    sed '/^[[:space:]]*$/d' | awk '!seen[$0]++'
}

# registry_image_alive <host>：该镜像站能否取到基础镜像的 manifest
# 判定规则（实测国内镜像站的两种响应）：
#   200                                  → 直接可取，可用
#   401 + WWW-Authenticate: Bearer ...   → 需要 token 握手，**Docker 自己能完成**，可用
#   403 且**没有** WWW-Authenticate 头    → 镜像站直接拒绝，Docker 也拿不到 token，不可用
#     （docker.xuanyuan.me / docker.nju.edu.cn 实测就是这种裸 403）
# 所以不能只看状态码：裸 403 和 401 对 Docker 的意义完全不同。
# 有些镜像站首次握手慢（Cloudflare 冷启动），所以重试一次再判定。
registry_image_alive() {
  local host="$1" repo="library/golang" tag="$BASE_IMAGE_TAG" code hdr i
  [ -n "$host" ] || return 1
  i=0
  while [ "$i" -lt 2 ]; do
    i=$((i + 1))
    # 同时抓响应头（要判断有没有 WWW-Authenticate）与状态码
    hdr="$(curl -s -o /dev/null -m 15 -D - \
      -H 'Accept: application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.docker.distribution.manifest.v2+json' \
      -w '\n%{http_code}' \
      "https://${host}/v2/${repo}/manifests/${tag}" 2>/dev/null)" || hdr=""
    # 去掉 CR，取最后一行纯数字（= 状态码）；响应头行不会误伤
    hdr="$(printf '%s' "$hdr" | tr -d '\r')"
    code="$(printf '%s\n' "$hdr" | grep -E '^[0-9]{3}$' | tail -n1)"
    case "$code" in
      200) return 0 ;;
      401)
        # 需要 token：只有带 WWW-Authenticate: Bearer 才说明镜像站愿意发 token
        if printf '%s' "$hdr" | grep -qi '^www-authenticate:[[:space:]]*bearer'; then
          return 0
        fi
        ;;
      403)
        # 裸 403（无 WWW-Authenticate）= 镜像站不可用；带 Bearer 的 403 仍算可用
        if printf '%s' "$hdr" | grep -qi '^www-authenticate:[[:space:]]*bearer'; then
          return 0
        fi
        ;;
    esac
    sleep 2
  done
  return 1
}

# 基础镜像全名：docker.io 用简名，其它镜像站补 /library 前缀
image_ref_for() {
  local host="$1"
  if [ -z "$host" ] || [ "$host" = "docker.io" ]; then
    printf 'golang:%s' "$BASE_IMAGE_TAG"
  else
    printf '%s/library/golang:%s' "$host" "$BASE_IMAGE_TAG"
  fi
}

# 预拉基础镜像：国内大层慢，先显式拉一次并显示进度，比让 build 干等友好得多
prepull_base_image() {
  local host ref
  for host in $(candidate_registries); do
    ref="$(image_ref_for "$host")"
    info "拉取基础镜像：$ref"
    if [ "$DRY_RUN" = "1" ]; then
      run docker pull "$ref"
      return 0
    fi
    if run_timeout "$PULL_TIMEOUT" docker pull "$ref"; then
      ok "基础镜像就绪：$ref"
      return 0
    fi
    warn "拉取失败或超时：$ref"
  done
  return 1
}

# ---------- 构建镜像 ----------
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

  info "版本：$ver（构建与运行都基于 golang 镜像，首次拉取较慢；运行镜像自带 Go 工具链）"
  info "Go 模块代理：$GOPROXY_ARG"
  info "校验数据库：${GOSUMDB_ARG:-sum.golang.org}（查询经 GOPROXY 转发，国内可用）"
  info "工具链策略：${GOTOOLCHAIN_ARG:-auto}（auto=版本不够时自动经代理下载工具链）"
  info "Alpine 软件源：${APK_MIRROR:-官方}${APK_FALLBACK_MIRRORS:+ / 备用 $APK_FALLBACK_MIRRORS}"

  # 先把基础镜像拉下来：国内大层慢，显式拉一次能看清进度，也能顺便筛掉不用的镜像站
  if ! prepull_base_image; then
    warn "基础镜像预拉取失败，继续构建（build 会再试一次）"
  fi

  # docker build 的附加参数（老版本 docker 不认识 --progress，要先探测）
  local extra=""
  if docker build --help 2>/dev/null | grep -q -- '--progress'; then extra="$extra --progress=plain"; fi
  if [ "$BUILD_PULL" = "1" ]; then extra="$extra --pull"; fi
  if [ "$BUILD_NO_CACHE" = "1" ]; then extra="$extra --no-cache"; fi
  if [ -n "$BUILD_NETWORK" ]; then extra="$extra --network=$BUILD_NETWORK"; fi

  # 目标架构：默认不传任何架构相关参数（BuildKit 会按 --platform 自动注入
  # TARGETOS/TARGETARCH；Dockerfile 拿不到时回落到 `go env GOARCH`）。
  # ⚠️ 跨架构**绝不能**只传 --build-arg TARGETARCH：那只会改「编译出来的二进制」，
  #    两个 FROM 仍然按宿主架构拉基础镜像，最终镜像的平台标记也是宿主架构，
  #    跑到目标机器上就是 exec format error。必须用 --platform 让基础镜像一起换平台。
  local arch_args="" builder="docker build"
  if [ -n "$TARGET_ARCH_ARG" ]; then
    if ! docker buildx version >/dev/null 2>&1; then
      die "--arch 需要 buildx（--platform 只有 BuildKit 支持）：请先安装 docker-buildx-plugin"
    fi
    builder="docker buildx build --load"
    arch_args="--platform ${TARGET_OS_ARG}/${TARGET_ARCH_ARG}"
    warn "跨架构构建 ${TARGET_OS_ARG}/${TARGET_ARCH_ARG}（需要 QEMU：docker run --privileged --rm tonistiigi/binfmt --install all）"
  else
    info "目标架构：自动（跟随宿主/--platform，宿主为 $(uname -m)）"
  fi

  # 逐个镜像站尝试：第一个构建成功就收工；失败时先判断是不是网络问题，
  # 网络问题才换下一个镜像站重试（编译错误重试没有意义，直接报错）
  local host ref log
  log="$(mktemp)"
  for host in $(candidate_registries); do
    ref="$(image_ref_for "$host")"
    info "使用基础镜像：$ref"
    # 这里所有 build-arg 都显式传，不用数组（CentOS 7 的 bash 4.2 空数组展开会报错）
    if run $builder \
        $extra \
        $arch_args \
        --build-arg "APP_VERSION=$ver" \
        --build-arg "GIT_COMMIT=${GIT_COMMIT:-}" \
        --build-arg "BUILD_TIME=${BUILD_TIME:-}" \
        --build-arg "GOPROXY=$GOPROXY_ARG" \
        --build-arg "GOSUMDB=$GOSUMDB_ARG" \
        --build-arg "GOTOOLCHAIN=$GOTOOLCHAIN_ARG" \
        --build-arg "GOLANG_IMAGE=$ref" \
        --build-arg "RUNTIME_IMAGE=$ref" \
        --build-arg "APK_MIRROR=$APK_MIRROR" \
        --build-arg "APK_FALLBACK_MIRRORS=$APK_FALLBACK_MIRRORS" \
        -t "$IMAGE_NAME" "$SOURCE_DIR" 2>&1 | tee "$log"; then
      REGISTRY="$host"
      ok "镜像构建完成：$IMAGE_NAME"
      rm -f "$log"
      return 0
    fi
    # 失败原因分类：网络/镜像站问题才值得换站点重试
    if grep -qiE 'failed to resolve source metadata|manifest unknown|toomanyrequests|unexpected EOF|i/o timeout|TLS handshake timeout|connection reset|connection refused|no such host|certificate verify failed|error pulling image|failed to pull image|dial tcp' "$log" 2>/dev/null; then
      warn "用 $host 构建失败（网络/镜像站问题），换下一个镜像站重试"
      continue
    fi
    warn "构建失败，错误日志末尾："
    tail -n 30 "$log" 2>/dev/null || true
    rm -f "$log"
    die "镜像构建失败（不是网络问题，请看上面的日志；完整日志见上面的输出）"
  done
  rm -f "$log"
  die "镜像构建失败（所有镜像站都试过了：可加 --registry 换镜像站，或 --official 用官方源；也可加 --check 查看网络）"
}

# ---------- 启动 / 更新容器 ----------
start_container() {
  local image="$1"
  # 旧容器带来的自定义变量先加载，后面的显式 -e 会覆盖同名项（docker 里 -e 优先于 --env-file），
  # 所以 VDL_CONFIG / GOPROXY 这些由脚本自己管的项永远以脚本为准。
  local env_file="${CARRY_ENV:-/dev/null}"
  # 日志限流：NAS/云主机的 docker 日志默认不封顶，采集/下载日志量不小，
  # 不限制的话 json-file 会把系统盘写满（与 docker-compose.yml 里的 logging 保持一致）。
  run docker run -d \
    --name "$CONTAINER_NAME" \
    --restart unless-stopped \
    --log-driver json-file --log-opt max-size=10m --log-opt max-file=3 \
    --ulimit nofile=65536:65536 \
    --env-file "$env_file" \
    -p "${PORT}:5200" \
    -v "${DATA_DIR}:/app/videodata" \
    -e "TZ=${TZ:-Asia/Shanghai}" \
    -e "VDL_CONFIG=/app/videodata/config.yml" \
    -e "GOPROXY=${GOPROXY_ARG}" \
    -e "GOSUMDB=${GOSUMDB_ARG}" \
    -e "GOTOOLCHAIN=${GOTOOLCHAIN_ARG}" \
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
    # 必须在 rm 之前取：重建容器会丢掉「手动 -e 加进去的」变量（VDL_UPDATE_TOKEN 等），
    # 先提取旧容器的 VDL_* 存成 env 文件，等会儿用 --env-file 原样带回去。
    CARRY_ENV="$(container_env_file)"
    if [ -n "$CARRY_ENV" ] && [ -s "$CARRY_ENV" ]; then
      info "沿用旧容器的自定义环境变量：$(sed 's/=.*//' "$CARRY_ENV" | tr '\n' ' ')"
    fi
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
  if [ -n "$CARRY_ENV" ]; then rm -f "$CARRY_ENV"; CARRY_ENV=""; fi
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
  printf '  基础镜像站：%s（golang:%s）\n' "${REGISTRY:-官方 Docker Hub}" "$BASE_IMAGE_TAG"
  printf '  Go 代理：%s\n' "$GOPROXY_ARG"
  printf '\n常用命令：\n'
  printf '  docker logs -f %s          # 看日志\n' "$CONTAINER_NAME"
  printf '  docker restart %s          # 重启\n' "$CONTAINER_NAME"
  printf '  sudo %s --yes             # 再次执行 = 更新到最新版本\n' "$0"
  printf '  sudo %s --check            # 网络/镜像源体检\n' "$0"
  printf '  docker tag %s %s && docker rm -f %s && docker run ...   # 回滚用旧镜像\n' "$IMAGE_PREV" "$IMAGE_NAME" "$CONTAINER_NAME"
}

# ---------- 主流程 ----------
main() {
  check_platform
  need_root "$@"
  detect_os

  if [ "$CHECK_ONLY" = "1" ]; then
    # 体检全程要用 curl：极简系统上没装的话，所有探测都会返回 000，看起来像全网挂了
    if ! has curl; then
      [ "$(id -u)" -eq 0 ] && { install_pkg curl || true; }
      has curl || warn "缺少 curl：下面的探测会全部显示 000（先装 curl 或用 root 再跑 --check）"
    fi
    check_docker_env
    check_network
    exit 0
  fi

  if [ "$NO_INSTALL" = "1" ]; then
    has docker || die "--no-install 指定了，但系统没有 docker"
    docker_ready || die "docker 命令存在但服务不可用，请先启动 Docker"
  elif docker_ready; then
    info "Docker 已安装：$(docker --version 2>/dev/null | head -n1)"
  elif has docker; then
    # 装了但服务没起来（NAS/云主机重启后很常见）：先尝试启动，别上来就重装
    warn "docker 命令存在但服务未就绪，先尝试启动服务（起不来再重装）"
    start_docker_service
    sleep 3
    if docker_ready; then
      ok "Docker 服务已启动：$(docker --version 2>/dev/null | head -n1)"
    else
      install_docker
    fi
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
  check_docker_env
  resolve_settings
  fetch_source
  build_image
  run_container
  wait_healthy
  summary
}

main "$@"
