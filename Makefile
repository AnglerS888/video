# 版本信息：唯一来源是仓库根目录 VERSION；commit/build time 由 git 与当前时间补齐。
# 三者通过 -ldflags 注入 internal/version，二进制在「信息」页与 -version 里显示，
# 也是在线更新判断「远端提交是否比本机构建新」的依据。
VER := $(shell cat VERSION 2>/dev/null | tr -d '\r\n')
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null)
BUILD_TIME := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
  -X videoDownLoad/internal/version.Version=$(VER) \
  -X videoDownLoad/internal/version.Commit=$(COMMIT) \
  -X videoDownLoad/internal/version.BuildTime=$(BUILD_TIME)

# 常用构建目标。
# ffmpeg 不再随二进制内嵌：运行时优先用系统已装的，缺失时由「信息」页按需下载到 videodata/bin/。
# 所以没有 nofmpeg 之类的构建开关了，产物大小与平台无关（约 12MB）。
.PHONY: all tidy web build windows linux linux-arm nas docker docker-multi run run-lowmem clean

all: build

tidy:
	go mod tidy

# 前端构建：Vue3 + Vite + TS + Tailwind，产物输出到 internal/web/static（已被 go:embed 打包）
# 产物随仓库提交，所以普通 go build 不需要 Node（想改前端时才需要跑这个）
web:
	cd web && npm install --registry=https://registry.npmmirror.com && npm run build

# 本机平台构建
build:
	go build -trimpath -ldflags="$(LDFLAGS)" -o bin/videodl.exe .

# Windows amd64
windows:
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/videodl-windows-amd64.exe .

# Linux amd64
linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/videodl-linux-amd64 .

# Linux arm64（ARM 机型 NAS 用）
linux-arm:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/videodl-linux-arm64 .

# NAS 部署：一次产出 amd64 + arm64 两个静态二进制（直接丢到 NAS 跑，无需 docker）
# 目标机没装 ffmpeg 时，首次进「信息」页点一次下载即可（约 29MB，存到 videodata/bin/）
nas: linux linux-arm
	@ls -lh bin/ | grep linux

docker:
	docker compose build

# 多架构镜像（需要 buildx）
docker-multi:
	docker buildx build --platform linux/amd64,linux/arm64 -t videodl:latest .

run:
	go run . -addr :5200 -data ./videodata

# 低配模式本地试跑（等同 NAS 上的默认参数）
run-lowmem:
	go run . -addr :5200 -data ./videodata -lowmem

# 只清构建产物。数据目录 videodata/ 是唯一的库与下载文件，绝不在这里删
# （要清数据请自己手动操作，确认已备份）
clean:
	rm -rf bin
