#!/bin/bash
# ============================================================
# 影视库（videodl）- 一键部署脚本
# 自动下载最新代码 → 打包镜像 → 创建/更新容器
# ============================================================

set -e

# ---------- 配置 ----------
IMAGE_NAME="videodl"
CONTAINER_NAME="videodl"
PORT="5200"
CODE_URL="https://gitee.com/anglers/video/repository/archive/master.tar.gz"

# ---------- 颜色 ----------
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
NC='\033[0m'

log()  { echo -e "${GREEN}[INFO]${NC}  $(date '+%H:%M:%S')  $1"; }
warn() { echo -e "${YELLOW}[WARN]${NC}  $(date '+%H:%M:%S')  $1"; }
err()  { echo -e "${RED}[ERROR]${NC} $(date '+%H:%M:%S')  $1"; }

# ---------- 检查环境 ----------
check_env() {
    if ! command -v docker &>/dev/null; then
        err "Docker 未安装"
        exit 1
    fi
    if ! docker info &>/dev/null; then
        err "Docker 服务未运行"
        exit 1
    fi
    if ! command -v curl &>/dev/null; then
        err "curl 未安装"
        exit 1
    fi
}

# ---------- 主流程 ----------
main() {
    check_env

    echo ""
    echo -e "${GREEN}============================================${NC}"
    echo -e "${GREEN}  影视库（videodl）- 部署脚本${NC}"
    echo -e "${GREEN}============================================${NC}"
    echo ""

    # 1. 下载最新代码
    log "[1/4] 下载最新代码..."
    TMP_DIR=$(mktemp -d)
    trap "rm -rf '$TMP_DIR'" EXIT

    # ⚠️ 必须用命令行风格 UA：Gitee 对浏览器 UA 会返回登录/验证页（HTML 而不是 tar.gz）
    if ! curl -fsSL --connect-timeout 30 --max-time 120 \
         -A 'curl/8.10.1' "$CODE_URL" -o "$TMP_DIR/source.tar.gz"; then
        err "下载失败: $CODE_URL"
        exit 1
    fi
    log "  ✓ 下载完成"

    # 2. 解压（校验必须是 tar 包，防止拿到登录页）
    log "[2/4] 解压代码..."
    if ! tar -xzf "$TMP_DIR/source.tar.gz" -C "$TMP_DIR" 2>/dev/null; then
        err "解压失败（下载到的可能不是源码包，检查网络或手动下载）"
        exit 1
    fi
    SRC_DIR=$(ls -d "$TMP_DIR"/*/ 2>/dev/null | head -1)
    if [ -z "$SRC_DIR" ]; then
        err "解压后未找到源码目录"
        exit 1
    fi
    log "  ✓ 解压完成 -> $(basename "$SRC_DIR")"

    # 3. 构建镜像
    log "[3/4] 构建 Docker 镜像..."
    cd "$SRC_DIR"
    if ! docker build -t "$IMAGE_NAME" . ; then
        err "镜像构建失败"
        exit 1
    fi
    log "  ✓ 镜像构建完成"

    # 4. 更新容器
    log "[4/4] 更新容器..."
    if docker ps -a --format '{{.Names}}' | grep -q "^${CONTAINER_NAME}$"; then
        log "  停止旧容器..."
        docker stop "$CONTAINER_NAME" 2>/dev/null || true
        log "  移除旧容器..."
        docker rm "$CONTAINER_NAME" 2>/dev/null || true
    fi

    log "  启动新容器..."
    docker run -d \
        --name "$CONTAINER_NAME" \
        --restart=unless-stopped \
        -p "$PORT:5200" \
        -e TZ=Asia/Shanghai \
        -e VDL_CONFIG=/app/videodata/config.yml \
        -v "${CONTAINER_NAME}-data:/app/videodata" \
        "$IMAGE_NAME"

    # 5. 验证
    sleep 2
    if docker ps --format '{{.Names}}' | grep -q "^${CONTAINER_NAME}$"; then
        echo ""
        echo -e "${GREEN}============================================${NC}"
        echo -e "${GREEN}  部署成功！${NC}"
        echo -e "${GREEN}============================================${NC}"
        echo -e "  访问地址: ${YELLOW}http://localhost:${PORT}${NC}"
        echo -e "  数据卷:   ${YELLOW}${CONTAINER_NAME}-data（数据库/视频/配置）${NC}"
        echo -e "  查看日志: ${YELLOW}docker logs -f ${CONTAINER_NAME}${NC}"
        echo -e "  健康检查: ${YELLOW}curl http://localhost:${PORT}/healthz${NC}"
        echo -e "${GREEN}============================================${NC}"
    else
        err "容器启动失败，查看日志: docker logs $CONTAINER_NAME"
        exit 1
    fi
}

main
