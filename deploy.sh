#!/bin/bash
# ============================================================
# 影视库（videodl）- 一键部署 / 更新脚本
#
# 用法：
#   curl -fsSL https://gitee.com/anglers/video/raw/master/deploy.sh | bash
#   sudo ./deploy.sh                          # 交互：询问对外端口、数据目录
#   sudo ./deploy.sh --port 8080 --data /data/videodl
#   sudo ./deploy.sh --yes                    # 非交互：用默认值（已有容器则沿用）
#
# 再次执行 = 更新：默认沿用旧容器的端口与数据目录，直接回车即可。
# ============================================================

set -euo pipefail

# ---------- 配置 ----------
IMAGE_NAME="videodl"
CONTAINER_NAME="videodl"
CODE_URL="https://gitee.com/anglers/video/repository/archive/master.tar.gz"
DEFAULT_PORT="5200"
WORK_DIR="$(pwd)"

PORT=""
DATA_DIR=""
ASSUME_YES=0

# ---------- 颜色 ----------
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
NC='\033[0m'

log()  { echo -e "${GREEN}[INFO]${NC}  $(date '+%H:%M:%S')  $1"; }
warn() { echo -e "${YELLOW}[WARN]${NC}  $(date '+%H:%M:%S')  $1"; }
err()  { echo -e "${RED}[ERROR]${NC} $(date '+%H:%M:%S')  $1"; }
die()  { err "$1"; exit 1; }

usage() {
    cat <<'EOF'
用法：deploy.sh [选项]

  --port N       对外访问端口（容器内固定 5200，默认 5200）
  --data PATH    数据存储目录（数据库 / 视频 / 配置都在这里，不存在会自动创建）
  --yes, -y      非交互模式：全部用默认值（已有容器则沿用它的端口与数据目录）
  -h, --help     显示本帮助

示例：
  curl -fsSL https://gitee.com/anglers/video/raw/master/deploy.sh | bash
  sudo ./deploy.sh --port 8080 --data /data/videodl
EOF
}

# ---------- 参数 ----------
while [ $# -gt 0 ]; do
    case "$1" in
        --port)
            [ $# -ge 2 ] || die "--port 需要一个端口号"
            PORT="$2"; shift 2 ;;
        --data)
            [ $# -ge 2 ] || die "--data 需要一个目录路径"
            DATA_DIR="$2"; shift 2 ;;
        --yes|-y)
            ASSUME_YES=1; shift ;;
        -h|--help)
            usage; exit 0 ;;
        *)
            die "未知参数：$1（--help 查看用法）" ;;
    esac
done

# ---------- 交互（兼容 curl | bash） ----------
# 管道执行时 stdin 是脚本本身，不能直接 read；此时从 /dev/tty 读。
can_ask() {
    [ "$ASSUME_YES" = "1" ] && return 1
    [ -t 0 ] && return 0
    [ -t 2 ] && [ -e /dev/tty ] && return 0
    return 1
}

# ask <提示> [默认值] -> REPLY_VALUE
ask() {
    local prompt="$1" def="${2:-}" ans=""
    REPLY_VALUE="$def"
    can_ask || return 0
    if [ -t 0 ]; then
        if [ -n "$def" ]; then
            IFS= read -r -p "$prompt（直接回车用默认值 [$def]）: " ans || true
        else
            IFS= read -r -p "$prompt: " ans || true
        fi
    else
        if [ -n "$def" ]; then
            printf '%s（直接回车用默认值 [%s]）: ' "$prompt" "$def" >/dev/tty
        else
            printf '%s: ' "$prompt" >/dev/tty
        fi
        IFS= read -r ans </dev/tty || true
    fi
    [ -n "$ans" ] && REPLY_VALUE="$ans"
    return 0
}

valid_port() {
    case "$1" in
        ''|*[!0-9]*) return 1 ;;
    esac
    [ "$1" -ge 1 ] && [ "$1" -le 65535 ] 2>/dev/null
}

port_in_use() {
    if command -v ss &>/dev/null; then
        ss -ltn 2>/dev/null | awk '{print $4}' | grep -qE "[:.]$1\$"
    elif command -v netstat &>/dev/null; then
        netstat -ltn 2>/dev/null | awk '{print $4}' | grep -qE "[:.]$1\$"
    else
        return 1
    fi
}

# ---------- 环境检查 ----------
check_env() {
    command -v docker &>/dev/null || die "Docker 未安装，请先安装：curl -fsSL https://get.docker.com | sh"
    docker info &>/dev/null || die "Docker 服务未运行，请先启动：systemctl start docker"
    command -v curl &>/dev/null || die "curl 未安装"
    command -v tar &>/dev/null || die "tar 未安装"
}

# ---------- 已有容器信息（更新时作为默认值） ----------
has_container() {
    docker ps -a --format '{{.Names}}' 2>/dev/null | grep -qx "$CONTAINER_NAME"
}

container_port() {
    docker port "$CONTAINER_NAME" 5200/tcp 2>/dev/null | head -n1 | awk -F: '{print $NF}' || true
}

# 输出 "类型|名称|源路径"：bind -> bind||/path；volume -> volume|卷名|/var/lib/...
container_data_info() {
    docker inspect -f '{{range .Mounts}}{{if eq .Destination "/app/videodata"}}{{.Type}}|{{.Name}}|{{.Source}}{{end}}{{end}}' \
        "$CONTAINER_NAME" 2>/dev/null || true
}

# ---------- 配置：端口 + 数据目录 ----------
resolve_settings() {
    local dport="$DEFAULT_PORT" ddata="$WORK_DIR/videodata"
    local old_volume="" mount_info="" mtype="" mname="" msource=""

    if has_container; then
        local cp
        cp="$(container_port)"
        [ -n "$cp" ] && dport="$cp"

        mount_info="$(container_data_info)"
        if [ -n "$mount_info" ]; then
            mtype="$(printf '%s' "$mount_info" | awk -F'|' '{print $1}')"
            mname="$(printf '%s' "$mount_info" | awk -F'|' '{print $2}')"
            msource="$(printf '%s' "$mount_info" | awk -F'|' '{print $3}')"
            case "$mtype" in
                volume)
                    old_volume="$mname"
                    [ -n "$mname" ] && ddata="$mname" ;;
                bind)
                    [ -n "$msource" ] && ddata="$msource" ;;
            esac
        fi
        log "检测到已有容器 ${CONTAINER_NAME}（端口 ${dport}，数据 ${ddata}），本次将重建它"
    fi

    # 1) 对外端口
    if [ -z "$PORT" ]; then
        ask "对外访问端口（容器内固定 5200）" "$dport"
        PORT="$REPLY_VALUE"
    fi
    local tries=0
    while ! valid_port "$PORT"; do
        tries=$((tries + 1))
        if ! can_ask || [ "$tries" -ge 5 ]; then
            die "端口不合法：$PORT（应为 1~65535）"
        fi
        warn "端口不合法：$PORT（应为 1~65535）"
        ask "对外访问端口（容器内固定 5200）" "$dport"
        PORT="$REPLY_VALUE"
    done

    # 2) 数据目录
    if [ -z "$DATA_DIR" ]; then
        ask "数据存储目录（数据库/视频/配置，不存在会自动创建）" "$ddata"
        DATA_DIR="$REPLY_VALUE"
    fi
    [ -n "$DATA_DIR" ] || die "数据目录不能为空"

    # 旧容器用的是命名卷：默认值就是卷名，原样沿用，不做路径展开
    if [ -n "$old_volume" ] && [ "$DATA_DIR" = "$old_volume" ]; then
        DATA_MOUNT="$DATA_DIR"
        log "沿用旧数据卷：$DATA_MOUNT"
    else
        # 展开 ~ / 相对路径，去掉末尾斜杠
        case "$DATA_DIR" in
            "~") DATA_DIR="$HOME" ;;
            "~/"*) DATA_DIR="$HOME/${DATA_DIR#\~/}" ;;
        esac
        case "$DATA_DIR" in
            /*) ;;
            *) DATA_DIR="$WORK_DIR/$DATA_DIR" ;;
        esac
        DATA_DIR="$(printf '%s' "$DATA_DIR" | sed -e 's#/\./#/#g' -e 's#//*#/#g')"
        [ "$DATA_DIR" != "/" ] && DATA_DIR="${DATA_DIR%/}"

        # 不存在就自动创建
        if [ ! -d "$DATA_DIR" ]; then
            log "数据目录不存在，自动创建：$DATA_DIR"
            mkdir -p "$DATA_DIR" 2>/dev/null || die "创建数据目录失败：$DATA_DIR（请检查权限，或换一个目录）"
        fi
        [ -w "$DATA_DIR" ] || die "数据目录不可写：$DATA_DIR（试试 sudo，或换一个有写权限的目录）"
        DATA_MOUNT="$DATA_DIR"

        if [ -n "$old_volume" ]; then
            warn "原数据在命名卷 ${old_volume} 里；新目录 ${DATA_DIR} 是空的，旧数据不会自动迁移"
            warn "如需保留原数据：先备份卷内容（docker run --rm -v ${old_volume}:/from -v ${DATA_DIR}:/to alpine cp -a /from/. /to/）"
        fi
    fi

    if ! has_container && port_in_use "$PORT"; then
        warn "端口 $PORT 似乎已被其它程序占用，启动可能失败；可换一个端口重试"
    fi

    log "对外端口：$PORT    数据目录：$DATA_MOUNT"
}

# ---------- 主流程 ----------
main() {
    check_env

    echo ""
    echo -e "${GREEN}============================================${NC}"
    echo -e "${GREEN}  影视库（videodl）- 部署脚本${NC}"
    echo -e "${GREEN}============================================${NC}"
    echo ""

    # 1. 端口 / 数据目录（先问清楚，再开始下载构建）
    log "[1/4] 确认部署参数..."
    resolve_settings

    # 2. 下载最新代码
    log "[2/4] 下载最新代码..."
    TMP_DIR=$(mktemp -d)
    trap 'rm -rf "$TMP_DIR"' EXIT

    # 必须用命令行风格 UA：Gitee 对浏览器 UA 会返回登录/验证页（HTML 而不是 tar.gz）
    if ! curl -fsSL --connect-timeout 30 --max-time 180 \
         -A 'curl/8.10.1' "$CODE_URL" -o "$TMP_DIR/source.tar.gz"; then
        die "下载失败：$CODE_URL"
    fi
    log "  ✓ 下载完成"

    if ! tar -xzf "$TMP_DIR/source.tar.gz" -C "$TMP_DIR" 2>/dev/null; then
        die "解压失败（下载到的可能不是源码包，检查网络或手动下载）"
    fi
    SRC_DIR="$(find "$TMP_DIR" -mindepth 1 -maxdepth 1 -type d 2>/dev/null | head -n1 || true)"
    [ -n "$SRC_DIR" ] || die "解压后未找到源码目录"
    [ -f "$SRC_DIR/Dockerfile" ] || die "源码包不完整（缺少 Dockerfile）"
    log "  ✓ 解压完成 -> $(basename "$SRC_DIR")"

    # 3. 构建镜像
    log "[3/4] 构建 Docker 镜像（首次 2~5 分钟）..."
    cd "$SRC_DIR"
    if ! docker build -t "$IMAGE_NAME" . ; then
        die "镜像构建失败（国内网络可检查 Docker 镜像加速设置）"
    fi
    log "  ✓ 镜像构建完成"

    # 4. 重建容器
    log "[4/4] 重建容器..."
    if has_container; then
        log "  停止并移除旧容器..."
        docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
    fi

    log "  启动新容器..."
    docker run -d \
        --name "$CONTAINER_NAME" \
        --restart=unless-stopped \
        -p "${PORT}:5200" \
        -e TZ=Asia/Shanghai \
        -e VDL_CONFIG=/app/videodata/config.yml \
        -v "${DATA_MOUNT}:/app/videodata" \
        "$IMAGE_NAME" >/dev/null || die "启动容器失败（端口 $PORT 可能被占用，或数据目录不可用）"

    # 5. 健康检查
    log "  等待服务就绪..."
    local ok=0 i=0
    while [ "$i" -lt 30 ]; do
        i=$((i + 1))
        if curl -fsS --max-time 2 "http://127.0.0.1:${PORT}/healthz" >/dev/null 2>&1; then
            ok=1
            break
        fi
        sleep 2
    done
    if [ "$ok" != "1" ]; then
        err "启动后 60 秒内 /healthz 未通过，最近日志："
        docker logs --tail 50 "$CONTAINER_NAME" 2>&1 || true
        die "容器启动失败"
    fi

    local ip
    ip="$(hostname -I 2>/dev/null | awk '{print $1}' || true)"
    [ -n "$ip" ] || ip="localhost"

    echo ""
    echo -e "${GREEN}============================================${NC}"
    echo -e "${GREEN}  部署成功！${NC}"
    echo -e "${GREEN}============================================${NC}"
    echo -e "  访问地址: ${YELLOW}http://${ip}:${PORT}${NC}（本机 http://localhost:${PORT}）"
    echo -e "  数据目录: ${YELLOW}${DATA_MOUNT}${NC}（数据库/视频/配置）"
    echo -e "  查看日志: ${YELLOW}docker logs -f ${CONTAINER_NAME}${NC}"
    echo -e "  健康检查: ${YELLOW}curl http://localhost:${PORT}/healthz${NC}"
    echo -e "${GREEN}============================================${NC}"
}

main
