# videoDownLoad

自建影视资源采集、下载与播放的一体化服务。

对接标准苹果 CMS（`api.php/provide/vod/`）影视接口，把多个资源站的片库同步到本地 SQLite，
支持在线播放、m3u8 下载合并成 mp4、本地续播与局域网分享。单个二进制文件即可运行，
不依赖外部数据库，专为低配 NAS 做过资源占用优化。

> 程序本身**不内置、不预置任何采集接口**，所有片源都由使用者自行添加。

## 一键安装

Linux 服务器（已装 Docker）一条命令部署，脚本会询问对外端口和数据存储目录：

```bash
curl -fsSL https://gitee.com/anglers/video/raw/master/deploy.sh | bash
```

数据目录不存在会自动创建；再次执行就是更新，默认沿用原来的端口与数据目录。

## 功能

- **多接口采集** —— 添加任意数量的影视接口，只填域名，按域名自动补全接口路径
- **全量 / 增量同步** —— 全量可随时停止、断点续采；定时增量每小时只抓每个源最新的几页
- **同名多源合并** —— 同一个剧名在多个源都有时合成一条，详情页里按线路分组展示
- **在线播放** —— HLS 代理改写 m3u8 并转发分片，绕开源站无 CORS / 防盗链 / 伪装分片
- **下载** —— m3u8 分片并发拉取，ffmpeg 合并为 mp4，带进度、速度、暂停与续传
- **本地库** —— 按剧分组管理已下载内容，记录播放位置，支持续播与自动下一集
- **收藏** —— 按剧名收藏，显示源站更新时间是否变化
- **分享** —— 生成局域网「分享页」链接与二维码；链接带随机令牌和有效期（24 小时 / 7 天 / 永久），
  对方打开的是页面，可自行播放/下载，不会直接拿到视频文件地址
- **封面本地化** —— 抓取并压缩封面存到本地，页面只读本地文件；确实拉不到时显示内置占位图
- 搜索可按**片名 / 演员**切换范围（默认全部）
- 「接口文档」页把站点自身的 HTTP 接口列出来，可复制 curl


## 技术栈

| 层 | 选型 |
|---|---|
| 后端 | Go 1.26，标准库 `net/http`（Go 1.22+ 路由模式） |
| 数据库 | SQLite（`modernc.org/sqlite`，纯 Go 实现，无 CGO 依赖） |
| 前端 | Vue 3.5 + Vite 8 + TypeScript + Tailwind CSS 4 + vue-router |
| 播放器 | DPlayer + hls.js（按需懒加载） |
| 打包 | 前端产物通过 `go:embed` 打进可执行文件；ffmpeg 不再内嵌，按需下载（见下） |

前端构建产物已随仓库提交，`go build` 不需要 Node 环境。只有修改前端时才需要 `make web`。

## 快速开始

### 二进制运行

```bash
make build                    # 产出 bin/videodl.exe（当前平台）
./bin/videodl                 # 首次运行会在当前目录生成 config.yml
```

打开 <http://localhost:5200>。端口、数据目录、数据库等在 `config.yml`，采集/下载/封面/ffmpeg 在「信息」页改（存到数据目录的 `ui.yml`）：

```bash
cp config.example.yml config.yml   # 精简模板：默认值与低配/标准预设都在代码里
```

两份配置都是 **YAML**（用 `#` 写注释）；旧版 JSON 配置（含 `//` 注释的）仍然能读。
容器里用 `VDL_CONFIG` 指向 volume 上的 `config.yml`（compose 已配好）。

### Docker

```bash
docker compose up -d
```

仓库里的 `docker-compose.yml` 已经配好数据目录（`./videodata`）、端口（`5200`）、
Go 模块代理（`goproxy.cn` / `goproxy.io`）和阿里云 apk 源，`docker compose up -d` 直接可用。
基础镜像 `golang:1.26-alpine` 来自 Docker Hub，国内网络慢时给 Docker 配镜像加速即可。

需要调整时用环境变量覆盖，例如：

```bash
VDL_PORT=8080 VDL_GOPROXY=https://proxy.golang.org,direct docker compose up -d --build
```

> ⚠️ compose 只认 `VDL_PORT` / `VDL_GOPROXY` 两个变量，且故意不用 `GOPROXY` 这些名字：
> 那是宿主机 Go 环境变量的名字，`go env -w GOPROXY=off` 或 export 过的人
> 会把本机设置带进容器（容器内「立即更新」编译会直接失败）。

### Linux 一键部署 / 更新（推荐）

```bash
# 一键执行：询问对外端口和数据目录
curl -fsSL https://gitee.com/anglers/video/raw/master/deploy.sh | bash

# 或者先下载再执行
chmod +x deploy.sh
sudo ./deploy.sh                       # 交互式：询问对外端口、数据存储目录
sudo ./deploy.sh --port 8080 --data /data/videodl
sudo ./deploy.sh --yes                 # 非交互：默认 5200 + 当前目录下的 videodata
```

脚本会依次做：

1. **检查 Docker / curl / tar**（未安装会提示安装命令，不会自动改系统）；
2. **确认部署参数**：询问对外端口和数据存储目录 —— 目录不存在自动 `mkdir -p`，
   相对路径按当前目录展开，端口校验 1~65535；
3. **下载 Gitee 最新源码**（固定命令行 UA，避免拿到登录页 HTML）；
4. **构建镜像** `videodl`；
5. **重建容器**：`--restart unless-stopped`、`-p <端口>:5200`、
   `-v <数据目录>:/app/videodata`、`VDL_CONFIG=/app/videodata/config.yml`；
6. **等 `/healthz` 通过**后输出访问地址；60 秒没过会打印容器日志。

**再次执行就是更新**：检测到已有容器时，默认沿用它的端口和数据目录，直接回车确认即可。
旧版本用的是命名数据卷（`videodl-data`），会原样沿用；如果手动输入了新目录，旧数据不会自动迁移，
脚本会提示迁移命令，请先备份再切换。

首次构建正常 2~5 分钟，之后的更新构建通常 1~2 分钟。

> Docker 未安装时先装：`curl -fsSL https://get.docker.com | sh`（国内网络可先配置镜像加速）。
> 基础镜像 `golang:1.26-alpine` 来自 Docker Hub；Go 模块代理、apk 源已在 Dockerfile 里配成国内源。


镜像不再内置 ffmpeg（二进制约 12MB，不再是 40MB）。运行时的解析顺序是：

1. 环境变量 `VDL_FFMPEG` 指向的可执行文件；
2. 数据目录 `videodata/bin/ffmpeg`（程序下载后放在这里，重启/重建容器只要 volume 在就复用）；
3. 系统 `PATH` 里的 `ffmpeg`（以及 `/usr/local/bin`、`/opt/homebrew/bin` 等常见位置）；
4. 都没有 → 打开「信息」页会显示 **未安装**，点「下载 ffmpeg」从国内镜像（npmmirror）下载当前平台
   的静态包（约 29MB），自动解压、`chmod 755`、跑一次 `ffmpeg -version` 校验后直接启用，**不用重启**。

想彻底离线就自己装：`docker run ... ` 里加 `apk add ffmpeg` 重打镜像，或手动把 ffmpeg 放进
`videodata/bin/`。状态与进度都在「信息」页，失败会显示原因并可点「重新下载」。

构建阶段默认走国内 Go 模块代理，且**串联多个代理自动兜底**：

```
GOPROXY=https://goproxy.cn|https://mirrors.aliyun.com/goproxy/|https://mirrors.cloud.tencent.com/go/|https://goproxy.io|direct
```

竖线 `|` 的含义是「前一个代理出错（超时 / DNS 抖动）也立刻换下一个」；逗号 `,` 只在
前一个返回 404 时才换 —— 国内网络抖，必须用竖线。`proxy.golang.org` 在国内连不上，
会报 `dial tcp ...:443: i/o timeout`。

**校验数据库保持开启（`GOSUMDB=sum.golang.org`），不要改成 `off`。**
`sum.golang.org` 本身国内连不上，但 Go 会把 sumdb 查询**转发给 GOPROXY 里的代理**
（`<代理>/sumdb/sum.golang.org/lookup/...`），实测 goproxy.cn / 阿里云 / 腾讯云都返回 200，
所以国内照样能用。

反例（**会失败**）：`GOSUMDB=off` 时，Go 想自动补齐缺失的工具链
（`golang.org/toolchain`）会走 sumdb 校验，被直接拒绝：

```
go: download go1.26.9: golang.org/toolchain@v0.0.1-go1.26.9.linux-amd64:
    verifying module: checksum database disabled by GOSUMDB=off
```

这会让「基础镜像 Go 版本略旧」的场景直接构建失败。海外机器可以覆盖代理：

```bash
docker build --build-arg GOPROXY=https://proxy.golang.org,direct -t videodl:latest .
```

（`GOSUMDB` 在 Dockerfile 里固定为 `sum.golang.org`，不需要改。）

> 想换基础镜像 / 软件源，直接改 `Dockerfile` 里的 `FROM` 和 `ARG APK_MIRROR`
> （apk 源也可以用 `docker build --build-arg APK_MIRROR=dl-cdn.alpinelinux.org .` 临时切回官方）。
>
> 基础镜像的 Go 版本低于 `go.mod` 要求时，`GOTOOLCHAIN=auto`（Dockerfile 默认）会经
> `GOPROXY` 自动下载对应工具链（约 70MB，一次性开销）；完全离线时把 Dockerfile 里的
> `GOTOOLCHAIN` 改成 `local`，此时版本不够会直接构建失败。
>
> **目标架构不用配**：`docker build` 在目标机器上构建，产出就是该机器的架构；
> 多架构用 `docker buildx build --platform linux/amd64,linux/arm64`（基础镜像会按目标平台拉取）。
> ⚠️ 不要在 Dockerfile / `docker-compose.yml` 里写死 `TARGETARCH=amd64` —— ARM 机器
> （部分飞牛/群晖 NAS）上会编出 x86 二进制，容器起来直接 `exec format error`。
> 要换架构请换**平台**：`DOCKER_DEFAULT_PLATFORM=linux/arm64 docker compose build` 或
> `docker buildx build --platform linux/arm64 --load`。

### 第一次使用

> 首次启动片库是空的，**需要自己添加接口**。

1. 进「来源」页，点添加，**只填域名即可** —— 例如 `https://www.example.com`，
   后端会自动补成 `https://www.example.com/api.php/provide/vod/`
2. 点「测试」确认接口通（显示 HTTP 状态、页数、条数）
3. 点「全量采集」把片库同步下来（几万部片子通常几十分钟到数小时，可随时停止）
4. 回「影视库」浏览 / 搜索，点进详情页可在线播放或下载

## 使用说明

### 页面

| 页面 | 路径 | 说明 |
|---|---|---|
| 影视库 | `/` | 搜索、按年份/地区/类型筛选，同名多源合并展示 |
| 详情 | `/v/:id` | 多源多线路、分集列表，可单集下载或整剧下载 |
| 本地 | `/local` | 已下载完成的内容，按剧分组；支持单集/整部删除（文件与记录一起删） |
| 下载 | `/downloads` | 未完成任务：进度、速度、暂停/继续/删除，可「优先下载」插队 |
| 观看历史 | `/history` | 全部播放记录（进度、上次观看时间），可只看未看完、移除单条或清空 |
| 播放（本地） | `/watch/:id` | 播放已下载的 mp4，自动续播、自动下一集；← → 切集；倍速会被记住 |
| 在线播放 | `/online/:vid` | 直接播源站 m3u8，不占磁盘；可换线路 |
| 分享页 | `/s/:token` | 发给别人的独立页面（无站点导航），可播放/下载，同剧其它集可直接切换；令牌过期即失效（旧数字 id 链接仍兼容） |
| 收藏 | `/favorites` | 按剧名收藏，提示源站是否有更新 |
| 来源 | `/sources` | 接口增删改、连通性测试、全量/增量/停止、自动增量开关 |
| 接口文档 | `/api-docs` | 站点自身 HTTP 接口一览（方法、路径、参数），可复制 curl |
| 网站信息 | `/info` | 版本与在线更新、运行环境、数据占用、磁盘、内存、数据库空闲页、ffmpeg 安装、封面缓存检查 |

### 采集策略

- **每页 100 条**（`limit=100`；部分源站会忽略该参数、固定返回 20 条，此时按源站返回的页数走）
- **每个接口 6 个分页线程**（`-pages`），页与页之间随机等 **3~5 秒**，避免把源站打崩
- **失败重试**：单页失败等 **180 秒**重试同一页；**连续 3 轮**都失败才终止该接口，并保存断点
  - 计数按「轮」不按「次」：一批并发请求同时失败只算一轮
  - 等待期间进度区显示「第 N 页抓取失败，180 秒后重试（连续 1/3 轮）」；恢复后提示自动消失
- **断点续采**：中途停止或失败，再点「全量采集」从断点继续；已完整跑完（`completed=1`）时则从第 1 页重新开始
- **JSON / XML 都支持**：按响应内容自动识别（看 `<?xml`/`<rss`/`<list>` 开头），无需选择格式；
  XML 站点填完整接口地址即可（如 `https://域名/api.php/provide/vod/at/xml/`，
  地址里的 `at/xml` 子路径会被保留，不会被改回 JSON）；声明为 GBK/GB2312 的 XML 会自动转 UTF-8
- **两种接口路径写法都认**：`/api.php/provide/vod/…`（常见）与 `/api/provide/vod/…`（部分站点去掉了 `.php`，
  如 `api.juliang.live`）。只填域名时默认补 `.php` 版本；若站点只有另一种写法，
  首次「测试」或采集会自动换到可用地址并回写接口表，之后不再多一次 404
- **添加前可以先「测试」**：填域名点「测试」，后端依次试 4 种常见地址（.php / 无 .php × JSON / XML），
  返回能用的地址、格式、页数/条数，以及 detail 能否取到播放地址；「添加」会直接用测试出来的地址
  （预检只探测、不写库）
- **只入库 m3u8**：源站常把网页分享页（`/share/<hash>`，返回 HTML）塞进播放列表，这类地址会被丢弃
- **播放地址去重**：同一部片同一条线路里，重复出现的地址只保留第一条（源站有时把相邻两集给同一个 m3u8）
- **两个开关别混**：接口「启用」= 是否参与任何采集；「自动增量」= 只控每小时定时任务，关掉后仍可手动增量/全量

### 在线播放原理

源站 m3u8 基本不带 CORS 头，浏览器直连会被拦；部分源还把 TS 分片伪装成 `.jpeg`。
所以播放统一走后端代理 `GET /api/live?u=<m3u8 绝对地址>`：

- 播放列表读成文本，把每行 URI 改写成代理地址（相对路径按最终 URL 解析）
- 分片检测伪装头（JPEG/PNG 等前缀 + TS 同步头扫描），剥掉后再转发，`Content-Type` 纠正为 `video/mp2t`
- 分片响应的 `Content-Length` / `Content-Range` 不回写（长度已变），并忽略 Range 回全量
- 前端强制走 hls.js（`customType`），只有没有 MSE 的 iOS Safari 才回退原生播放

### 下载

- 只接受 m3u8 地址；分片并发拉取（单任务 `-seg-concurrency`，默认 16），支持 AES-128 加密流
- **一次只跑一集**（固定串行，`-dl-workers` 传大于 1 会被忽略），避免多路转码把 NAS CPU 打满
- 全部集下完后由 ffmpeg 合并为 mp4；合并有进度输出，**90 秒无输出会被看门狗杀掉**并标记失败，可点继续重试
- 任务支持暂停 / 继续 / 批量操作，删除会连带删除半成品文件
- **优先下载**：排队/暂停/失败的任务可以点「优先」，当前正在下的那一集结束后立刻下它；
  列表里会显示「优先」标记
- **本地删除**：本地页每一集可用「×」单独删除、整部可用「删除」批量删除；
  文件存在就删文件，文件已不存在则只删记录（两种都成功，不会报错）

### 分享

- 本地播放页（`/watch/:id`）点分享，生成 `/s/{token}` 分享页链接与二维码；首页与详情页的二维码分享的是页面本身
- 可选有效期：永久（默认）/ 24 小时 / 7 天（对应 `?hours=0` / `24` / `168`，上限一年）；过期后分享页与文件都不再可访问
- 分享页不直接暴露视频文件地址，只给播放/下载按钮；同剧其它已下载集可直接切换
- 本地播放走短期播放令牌（`/api/play-token`，6 小时，进程内存态），刷新页面会自动重新获取

### 多源标注

同一部剧如果从多个接口都下载过，列表里会显示「多源 N」标记，
鼠标悬停可看到具体来源；每集播放按钮的 tooltip 也会带「来源：xxx」。

### 封面

- 采集时把封面下载并压缩（480px 宽、质量 80）存到 `videodata/img/<hash 前两位>/<hash>.jpg`
- `/api/img` **只返回本地已缓存的图**，没有就返回 `204`；列表接口同时返回同名的其它源封面供前端换源
- 前端加载不到封面时显示内置占位图 `web/public/doudi.png`（构建后位于站点根路径 `/doudi.png`）
- 下载全局限速：**每张间隔 500~1000ms**，所有协程共用一条队列；遇到 `429/403` 时按 `Retry-After`
  （默认 30 秒）暂停整个队列，等负缓存过期后再试
- **检查封面缓存**（信息页）：逐条确认本地缓存，缺失的按「同剧其它源已缓存 → 重新下载 → 图片代理兜底」
  补齐；同一图床连续失败 5 次就整轮跳过，代理请求全局串行、每 2~3 秒一次。进度和统计都在信息页，
  也可以直接调接口（`/api/cover/fix`，见下文）
- **图片代理**：页面填 `cover.img_proxy`（如 `https://wsrv.nl/?url=`）后，取图**优先走代理**，失败再直连；
  留空则只用直连。代理来源优先级：页面配置 > 环境变量 `VDL_COVER_PROXY` > 内置 `https://wsrv.nl/?url=`

## 版本与在线更新

版本号的唯一来源是仓库根目录的 **`VERSION`** 文件；构建时（Makefile / Dockerfile）会把它、
git 提交哈希和构建时间一起通过 `-ldflags` 注入 `internal/version`：

```bash
./videodl -version
# videodl v1.4.1 (11d5782)
#   commit: 11d5782
#   build : 2026-10-21 00:10:00
#   go     : go1.26.0 linux/amd64
#   mode   : Docker 容器
```

「信息」页有**版本与更新**卡片：显示当前版本/提交/构建时间，可一键检查是否有新版本。

**自动更新只在 Docker 容器内提供**；直接运行（单个二进制 / NAS 裸跑）只显示 Gitee 仓库链接，
由用户自己下载最新版本手动替换（数据目录和 `config.yml` 都不受影响）。

### 怎么判断有没有更新

仓库没有 release / tag，所以按三个信号判断，任一命中即提示有更新：

1. 远端 `VERSION` 文件里的版本号大于本机；
2. 本机构建时记录了提交哈希，且与远端 `main` 最新提交不同；
3. 本机没有提交哈希（直接 `go build`）时，远端提交时间晚于本机构建时间（2 分钟容差）。

检查走 Gitee OpenAPI（`https://gitee.com/api/v5/repos/anglers/video/commits/master`），
版本号读 `https://gitee.com/anglers/video/raw/master/VERSION`。
**只支持 Gitee**（GitHub 仓库已删除，不再做镜像回退）。

### 更新流程（仅 Docker 容器）

源码包来自 `https://gitee.com/anglers/video/repository/archive/master.zip`，
下载后解压到 `<数据目录>/update/src`，并校验 `go.mod` 与 `internal/web/static/index.html`
是否存在（防止把登录/验证网页当成源码）。

运行镜像自带 Go 工具链（基座是 golang，见 Dockerfile），所以「立即更新」是全自动的：

1. 下载并解压最新源码到数据目录；
2. 容器内 `go build` 编译新二进制（模块/构建缓存在 `update/gomodcache`、`update/gocache`，
   下次只增量编译），并用 `-version` 自检；
3. 原子替换 `/app/videodl` 后**进程内原地 exec 新二进制**（PID 不变，容器不会退出，
   也不需要容器配 `restart` 策略）；exec 失败才退出进程，交给容器重启策略兜底——
   全程不用登录宿主机，数据目录（volume）不受影响。

> 编译时内存开销比日常运行大不少（modernc/sqlite 很吃内存），建议给容器留够 2G 以上
> （compose 默认不设上限，需要时自己加 `mem_limit`）；编译并发压到 `-p 2`、GOGC=50。
> 弱机上编译可能要几分钟，属正常现象。

> 容器内自更新只替换**容器里**的程序文件：`docker restart` / 重启 NAS 后仍是新版本，
> 但 `docker compose up -d --force-recreate` 或重新 `build` 镜像会回到镜像里那份旧版本。
> 想把版本固化进镜像，仍需要在宿主机执行一次 `docker compose build && docker compose up -d`。

如果把 Dockerfile 的运行阶段换成不含 `go` 的精简镜像（如 `alpine:3.20`），
在线更新自动退回老模式：只把源码落到 `/app/videodata/update/src`，
由你在宿主机项目目录执行：

```bash
git pull                                   # 或把 update/src 同步到项目目录
docker compose build && docker compose up -d
```

**直接运行（非容器）**

不提供自动更新，接口也会直接拒绝。信息页只给「打开 Gitee 手动更新」按钮，流程是：

1. 到 <https://gitee.com/anglers/video> 下载最新版二进制（自己编译也行）；
2. 停掉旧进程，用新文件替换原程序；
3. 重新启动（配置和数据目录都在 `config.yml`/`videodata`，不用动）。

> 想升级发行包/自己发布的二进制：在 Gitee 上传 Release 后把下载地址告诉我，可以再加一条
> 「直接下载二进制替换」的路径；目前仓库只有源码包。

> 匿名下载源码包时 Gitee 可能要求登录/验证（返回网页而不是 zip）。
> 遇到这种情况可以用环境变量 `VDL_UPDATE_TOKEN` 提供 Gitee 私人令牌，程序会改走官方 `zipball` 接口
> （更新源固定为 `anglers/video@master`，配置里不再保留 update 段）。

> 更新源固定为 Gitee 的 `anglers/video@master`（不再放在配置里）；想换 fork 需要改代码里的 `version.Repo`。

> 「下载最新源码」按钮**只有检测到新版本时才可点**；进信息页会自动检查一次（30 分钟内不重复查）。

## 数据目录

所有状态都在 `data_dir` 指定的目录里，迁移或备份只需要拷这个目录：

```
videodata/
├── videodl.db              # SQLite 库（影片、播放地址、下载记录、观看历史、收藏、分享令牌）
├── videodl.db-wal          # WAL（运行中才有）
├── ui.yml                  # 页面可改的运行配置（采集 / 下载 / 封面 / ffmpeg）
├── img/                    # 封面缓存，按哈希分片：img/3f/3fa2...9c.jpg
├── videos/                 # 下载好的 mp4
├── bin/                    # 按需下载的 ffmpeg（信息页点「下载」后才有，约 80MB 解压后）
└── update/                 # 在线更新缓存：src/ 源码、gomodcache/gocache 编译缓存、done.json（可随时删）
```

目录名历史：`data/` → `videos_data/` → **`videodata/`**。启动时如果只找到旧目录名（且里面有库），
会自动改名迁移过来；改名失败（被占用 / 跨盘）则继续使用旧目录，保证程序能起来。

## 配置文件

**没有命令行参数**（只保留 `./videodl -version` 打印版本后退出）。配置拆成两个文件：

| 文件 | 位置 | 内容 | 生效方式 |
|---|---|---|---|
| `config.yml` | 见下方查找顺序 | `advanced` 段：端口、数据目录、日志、内存、数据库 | 改完**重启** |
| `ui.yml` | **数据目录**（`<data_dir>/ui.yml`） | 采集 / 下载 / 封面 / ffmpeg | 「信息」页改，**保存即生效** |

`ui.yml` 跟着数据走：换机器、换容器只要带上数据目录，并发设置就跟过去了；
第一次运行没有它会按当前生效值自动生成一份。

### 文件位置（config.yml）

按顺序查找，都找不到就在第一个可写位置生成一份带默认值的 `config.yml`：

1. 环境变量 `VDL_CONFIG` 指定的路径（不存在会在该处生成；**容器里推荐指向 volume**，见 compose）
2. 当前目录 `./config.yml`
3. 程序所在目录 `config.yml`
4. 当前目录 `videodata/config.yml`

文件是 **YAML**（用 `#` 写注释）；旧版 JSON 配置（含 `//`、`/* */` 注释）仍然能读，读取前会剥掉注释。
仓库里只附了一份 `config.example.yml`（精简模板）。
`ui.yml` 由程序首次启动时自动生成，之后由页面维护，不需要模板。
**默认值、标准预设、低配预设都写死在代码里**（`internal/config`），配置文件里不重复这些数值。

### 示例

`config.yml`（只放需要重启的项）：

```yaml
advanced:
  server:
    addr: ":5200"

  # 数据目录：SQLite、封面、下载的视频、ui.yml、ffmpeg 缓存都在这里
  data_dir: "./videodata"
  log_level: "info"          # debug / info / warn / error

  # 低配模式：先套一套 NAS 友好的默认值（主要影响 ui.yml 里那几项的初值）
  lowmem: false
  mem_limit_mb: 0            # Go GC 软上限（MiB），0=不限；lowmem 时默认 768
  max_procs: 0               # GOMAXPROCS，0=自动；lowmem 时默认 min(核数,3)

  db:
    cache_mb: 64
    mmap_mb: 1024
    conns: 4
    page_size: 0
    vacuum_pct: 15
```

`<数据目录>/ui.yml`（页面维护，保存即生效）：

```yaml
collect:
  pages: 6
  sources: 8
  incr_every: "1h"     # 1h / 30m / 1h30m
  incr_pages: 5        # 0=不限

download:
  workers: 1             # 同时下载的集数（实际固定串行）
  seg_concurrency: 16    # 单任务分片并发
  seg_budget: 0          # 全局分片并发，0=workers × seg_concurrency
  min_free_mb: 2048      # 磁盘剩余空间保护阈值，0=关闭

cover:
  workers: 2
  concurrency: 4
  img_proxy: ""          # 图片代理前缀，如 https://wsrv.nl/?url=

ffmpeg:
  threads: 0             # 0=默认；NAS 建议 1~2
  reencode: true
  verify: true
```

> 旧版结构（全部平铺在一个 `config.yml` 里，或 `ui`/`advanced` 两段写在同一个文件）
> **仍然能读**；从「信息」页保存一次后，页面项会自动落到 `ui.yml`。

### 字段说明

下表里 `collect.*` / `download.*` / `cover.*` / `ffmpeg.*` 在 **`ui.yml`**（页面可改、保存即生效），
其余（`server.*` / `data_dir` / `log_level` / `lowmem` / `mem_limit_mb` / `max_procs` / `db.*`）
在 **`config.yml` 的 `advanced` 段**（改完重启）。

| 字段 | 默认 | 说明 |
|---|---|---|
| `server.addr` | `:5200` | HTTP 监听地址 |
| `data_dir` | `./videodata` | 数据目录；不写时自动查找已有 `videodl.db` 的目录并迁移旧名 |
| `log_level` | `info` | 日志级别 |
| `lowmem` | `false` | 低配模式（NAS 推荐）：下调并发、关全量重编码、限制 GC 内存 |
| `mem_limit_mb` | `0` | Go GC 软内存上限（MiB），0=不限制 |
| `max_procs` | `0` | `GOMAXPROCS`，0=自动 |
| `collect.pages` | `6` | 每个接口的分页采集并发数 |
| `collect.sources` | `8` | 同时采集的接口数上限 |
| `collect.incr_every` | `1h` | 定时增量间隔（Go duration：`1h` / `30m` / `1h30m`） |
| `collect.incr_pages` | `5` | 定时增量每次同步的页数，0=不限 |
| `download.workers` | `1` | 同时下载的集数（固定串行） |
| `download.seg_concurrency` | `16` | 单任务分片并发 |
| `download.seg_budget` | `0` | 全局分片并发上限，0=自动 |
| `download.min_free_mb` | `2048` | 磁盘保护阈值（MiB）：低于该值拒绝新下载并暂停队列，0=关闭 |
| `cover.workers` | `2` | 封面预取协程数 |
| `cover.concurrency` | `4` | 封面同时连接/解码的并发上限 |
| `cover.img_proxy` | 空 | 图片代理前缀，如 `https://wsrv.nl/?url=`；填写后取图优先走代理，留空只用直连 |
| `ffmpeg.threads` | `0` | ffmpeg 线程数，0=默认；NAS 建议 1~2 |
| `ffmpeg.reencode` | `true` | 音画不同步时是否全量重编码（弱 CPU 上很慢，可关） |
| `ffmpeg.verify` | `true` | 合并后是否做音画同步自检 |
| `db.cache_mb` | `64` | SQLite 每连接页缓存（MiB）；4G 内存建议 `lowmem`（24） |
| `db.mmap_mb` | `1024` | 内存映射大小（MiB，0 关闭）；网络盘建议 0 |
| `db.conns` | `4` | SQLite 最大连接数（WAL 一写多读） |
| `db.page_size` | `0` | 页大小（如 `8192`）；0=不改。改已有库会先 VACUUM 重写一次 |
| `db.vacuum_pct` | `15` | 空闲页占比超过该值自动整理，0 关闭 |

### 在线调整（表单，保存即生效）

「信息」页的**运行配置**卡片只放**不用重启**的四组：

| 分组 | 项 | 生效时机 |
|---|---|---|
| 采集 | `collect.pages` / `sources` / `incr_every` / `incr_pages` | 下一轮采集、下一次定时增量（间隔立即重排） |
| 下载 | `download.workers` / `seg_concurrency` / `seg_budget` / `min_free_mb` | 下一集开始下载 |
| 封面 | `cover.workers` / `concurrency` / `img_proxy` | 立即 |
| 合并转码 | `ffmpeg.threads` / `reencode` / `verify` | 下一次合并 |

都是输入框 / 开关，不用写 JSON；顶部还有「标准预设」「低配预设」一键套用。点保存后
**立即热应用到运行中的组件**（正在跑的采集轮次、正在下的那一集不受影响），
同时写进 `<数据目录>/ui.yml`（旧文件备份为 `ui.yml.bak`），**不会动 config.yml**；
越界值会被夹到安全范围。

**需要重启的项**（`server.addr`、`data_dir`、`log_level`、`lowmem`、`mem_limit_mb`、
`max_procs`、`db.*`）在 `config.yml` 的 `advanced` 段，页面上不展示。

也可以走接口：`GET /api/config` 读取（合并 config.yml + ui.yml，返回 `config`、`path`、`ui_path`
与两套 `presets`）、`PUT /api/config` 保存 —— body 传 `{"config":{...}}`（缺省字段保持原值；
只含上面四组时写 `ui.yml` 并返回 `restart_required:false`，带上其它分组则同时写 `config.yml` 并返回 `true`）；
`{"text":"..."}` 仍然支持，用于整段原文写入（一律按需重启处理）。

### 低配模式怎么生效

`"lowmem": true` 时先取一套低配默认值（`pages=3`、`sources=2`、`cover.workers=1`、
`cover.concurrency=2`、`download.seg_concurrency=6`、`ffmpeg.threads=1`、`ffmpeg.reencode=false`、
`mem_limit_mb=768`、`max_procs=min(核数,3)`、`db.cache_mb=24`、`db.mmap_mb=256`、`db.conns=2`），
**再用文件里显式写过的字段覆盖**。注意 ui.yml 里那四组一旦存在就是显式值，
所以低配主要影响首次生成；之后想切换/微调用页面上的「标准预设 / 低配预设」更直接。

### 环境变量

只保留少量运行环境相关的开关，**常规配置一律走配置文件**：

| 变量 | 说明 |
|---|---|
| `VDL_CONFIG` | 指定配置文件路径（容器里指向 volume 上的 `config.yml`） |
| `VDL_LOWMEM=1` | 强制套用代码里的低配预设（等同页面的「低配预设」，不需要额外配置文件） |
| `VDL_HTTP_LOG=1` | 打印每个 API 请求的访问日志（默认只记慢请求） |
| `VDL_ADMIN_TOKEN` | 管理口令：设置后所有写接口要带 `X-Admin-Token` 头或 `?token=`（公网/半公网部署建议开） |
| `VDL_PUBLIC_URL` | 分享链接使用的站点根地址（如 `https://media.example.com`），不设则自动推断局域网 IP |
| `VDL_COLLECT_RETRY_DELAY` | 采集失败后的等待秒数，默认 `180` |
| `VDL_COLLECT_PAGE_DELAY` | 全量采集每页间隔，`3-5`（默认）/ `2` / `0` |
| `VDL_COVER_GAP_MS` | 封面下载间隔（毫秒），`500-1000`（默认）/ `800` / `0` |
| `VDL_COVER_PROXY` | 图片代理前缀（页面没配 `cover.img_proxy` 时使用），如 `https://wsrv.nl/?url=` |
| `VDL_COVER_PROXY_GAP_MS` | 代理请求间隔（毫秒），`2000-3000`（默认）/ `1000` / `0`（不限速） |
| `VDL_UPDATE_TOKEN` | Gitee 私人令牌：匿名下载源码包被要求登录时使用 |
| `VDL_FFMPEG` | 指定 ffmpeg 可执行文件路径（优先级最高） |

## 部署到 NAS

以 4G 内存、J4125 这类弱 CPU 的机型为例：

```bash
VDL_CONFIG=/vol1/videodata/config.yml ./videodl-linux-amd64
```

首次启动若系统没装 ffmpeg，进「信息」页点一次「下载 ffmpeg」即可（约 29MB，落在 `videodata/bin/`，
之后一直复用；删掉这个文件才会重新下载）。

或直接用仓库里的 `docker-compose.yml`，已经配好：

- 端口 `5200`（可用 `VDL_PORT=8080` 覆盖）、数据目录 `./videodata:/app/videodata`
- `init: true` + `stop_grace_period: 30s`，`docker stop` 时能让合并中的 ffmpeg 收尾
- 日志按 10MB × 3 份滚动，避免长期运行吃掉系统盘
- 健康检查用 `/healthz`（只做 `SELECT 1`），不用会扫全表的 `/api/system`
- Go 模块代理走国内（`goproxy.cn` / `goproxy.io`）、apk 走阿里云源；基础镜像
  `golang:1.26-alpine` 来自 Docker Hub，需要时自行配置镜像加速

弱机建议在 `config.yml` 里开 `lowmem: true`（或启动前设 `VDL_LOWMEM=1`）；需要限制容器内存时
自己加 compose 的 `mem_limit`（在线更新会在容器内 `go build`，建议至少留 2G）。

实测空载 / 小库时常驻约 80~120MB（不再内嵌 ffmpeg，省掉约 40MB 只读段），均不含转码时的额外开销；
采集与封面预取高峰期会明显上升，NAS 建议在配置里开 `lowmem`。

### 构建多平台二进制

```bash
make nas        # linux amd64 + arm64 静态二进制（体积一致，无需额外构建标签）
make windows    # windows amd64
```

ffmpeg 与平台无关地按需下载（Windows x64 / Linux x64 / Linux arm64 各有对应镜像包），
所以不再需要 `nofmpeg` 之类的构建开关，`make nas-light` 目标已删除。

## 数据库

### 1.4.1 结构升级

1.4.1 新增分享令牌表 `vod_share_token`，并给 `vod_download` 补 `priority` / `share_expires_at` 列、
`vod_info` 补 `src_time`、`vod_source` 补 `auto_incr`。**直接启动新版程序会自动补齐**；不想先启动服务时：

```bash
# 推荐：Go 工具（自动判断库结构、先备份，可反复执行）
go run ./tools/dbupgrade -db /path/to/videodata/videodl.db

# 或手动跑 SQL（先备份 videodl.db、videodl.db-wal、videodl.db-shm 三个文件）
sqlite3 videodl.db < migrations/upgrade-v1.4.1-current.sql
# 旧结构（vod_download 没有 ep_index 列）用 upgrade-v1.4.1-old-shape.sql
```

### 表

| 表 | 说明 |
|---|---|
| `vod_source` | 采集接口（名称、接口地址、启用、自动增量） |
| `vod_info` | 影片基础信息（`UNIQUE(source_id, source_vod_id)`） |
| `vod_detail` | 简介（拆表，主表更窄） |
| `play_host` | 播放地址主机字典（域名只存一次） |
| `vod_play` | 播放地址（`WITHOUT ROWID`，主键 `vod_id, play_from, ep_index`） |
| `vod_name_agg` | 剧名聚合表，列表/搜索走预聚合，避免全表 `GROUP BY` |
| `vod_download` | 下载任务 |
| `vod_sync_state` | 采集断点（全量/增量） |
| `vod_watch_history` | 观看进度（≥95% 记完成） |
| `vod_fav` | 收藏（按剧名） |
| `vod_share_token` | 分享令牌（随机串、失效时间；避免用自增 id 当凭据） |
| `app_meta` | 一次性维护任务的标记（如历史数据清理） |

### 设计要点

- **`vod_play` 用 `WITHOUT ROWID`**。这张表是库里最大的（百万级播放地址时占八成空间），
  表本身按主键组织成一棵 B 树，省掉额外一份完整索引；主键顺序为 `(vod_id, play_from, ep_index)`，
  "取某条线路的全部集"能直接走前缀。
- **播放地址拆成 `host_id` + `path`**，主机域名单独存 `play_host` 字典 —— 源站就那么几十个 CDN 域名，
  原来每条记录都要重复写一遍完整 URL。
- **空值不存**。`ep_name` 能由集号推导时留空，`created_at` 这类无人引用的列直接去掉。
- **连接池默认 4 个连接**（`db.conns`）。WAL 下可以「多读 + 单写」，慢查询不会堵住整站；
  写操作本身由 SQLite 串行化（`busy_timeout=10s` 兜住短暂排队）。但**任何 handler 里都不能在
  `rows` 还没读完时再发查询**（同一条连接会被占住，连接池耗尽后请求永久阻塞）。
- **播放/分享不靠自增 id**。本地播放要短期令牌（`/api/play-token`，6 小时、内存态），
  分享链接用带有效期的随机令牌（`vod_share_token`），枚举 `/play/1`、`/play/2` 拿不到文件。
- **自动整理空闲页**。采集时每条播放地址都是"先删后插"，SQLite 不会把删掉的页还给文件系统，
  库文件会一路涨。启动 2 分钟后、之后每 6 小时检查一次，空闲页占比超过阈值且服务空闲时自动 `VACUUM`。

### 手动整理

「网站信息」页的「数据目录」卡片里有「数据库空闲页」一栏，显示当前占比与可回收空间，
点「整理数据库」即可 —— 采集或下载进行中会被拒绝，无空间可回收时按钮置灰。

也可以用接口或命令行：

```bash
# 方式一：接口（采集或下载进行中会被拒绝）
curl -X POST http://<地址>:5200/api/system/vacuum

# 方式二：停服务后用 sqlite3
sqlite3 videodata/videodl.db "PRAGMA wal_checkpoint(TRUNCATE); VACUUM;"
```

`VACUUM` 会重写整个库，期间服务阻塞，大库在 NAS 上可能要几分钟。
`/api/sysinfo` 里的 `data.freelist_pct` 和 `data.reclaimable_bytes` 能看到当前可回收空间。

## HTTP 接口

所有接口返回 `{"ok": true, "data": ...}` 或 `{"ok": false, "msg": "..."}`。

**写接口防护**（`POST/PUT/DELETE` 统一由 `guard()` 处理）：

- 设了 `VDL_ADMIN_TOKEN` 时，必须带 `X-Admin-Token` 头或 `?token=`；
- 跨站 `Origin` 直接拒绝（CSRF）；
- 带请求体时 `Content-Type` 必须是 `application/json`；
- 请求体上限 1MB，批量数组最多 500 项。读接口不鉴权。

### 接口管理 / 采集

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/sources` | 接口列表（含每个源的影片数 / 剧名数 / 播放地址数 / 最近更新） |
| POST | `/api/sources` | 添加接口（只填域名即可） |
| PUT | `/api/sources/{id}` | 修改接口 |
| DELETE | `/api/sources/{id}` | 删除接口，**连带清空该源所有影片、播放地址、下载文件与封面** |
| GET | `/api/sources/{id}/test` | 测试连通性与分页 |
| POST | `/api/sources/probe` | 添加前预检（填域名，自动试 4 种地址，不写库）`{"url":"…"}` |
| POST | `/api/sources/{id}/auto-incr` | 开关「自动增量」`{"on":0\|1}` |
| GET | `/api/sources/export` | 导出全部接口为 JSON（换机/重装备份） |
| POST | `/api/sources/import` | 导入接口 JSON（按 `api_url` 去重） |
| POST | `/api/collect/full` | 全量采集（`?source_id=N` 单个，`?restart=1` 从头） |
| POST | `/api/collect/incr` | 增量采集 |
| POST | `/api/collect/stop` | 停止采集（`?source_id=N` 停单个，不传停全部） |
| GET | `/api/progress` | 采集进度 |

### 影片 / 封面 / 播放

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/videos` | 影视库列表（搜索 / 筛选 / 分页） |
| GET | `/api/videos/{id}` | 详情：多源、分线路、分集、单集下载状态 |
| GET | `/api/filters` | 筛选项（年份 / 地区 / 类型） |
| GET | `/api/img` | 封面图（只返回本地已缓存，没有则 `204`） |
| GET | `/api/live` | HLS 播放代理（改写 m3u8 + 转发分片） |
| GET | `/api/play-token` | 签发本地播放令牌（6 小时，内存态，重启失效） |
| GET | `/play/{id}` | 播放已下载的 mp4，支持 Range；本地播放要带 `?t=<播放令牌>`（或管理口令），`?dl=1` 作为附件下载；也接受分享令牌 `/play/{token}` |
| GET | `/api/downloads/{id}/share` | 生成分享页链接（`/s/{token}`）+ 直链；`?hours=24`（默认永久，可 `168`=7 天） |
| GET | `/api/share/base` | 分享用的站点根地址（本机访问时换成局域网 IP） |
| GET | `/api/share/{token}` | 分享页数据：封面、集数、同剧其它已下载集（旧数字 id 仍兼容） |

### 下载 / 观看

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/download` | 创建下载任务 |
| GET | `/api/downloads` | 下载任务列表 |
| POST | `/api/downloads/{id}/pause` | 暂停 |
| POST | `/api/downloads/{id}/resume` | 继续 |
| POST | `/api/downloads/{id}/retry` | 失败任务重试（重置为排队，已下好的分片复用） |
| POST | `/api/downloads/{id}/priority` | 优先下载：当前这一集下完后立刻轮到它 |
| DELETE | `/api/downloads/{id}` | 删除任务（连带删除半成品文件） |
| POST | `/api/downloads/batch` | 批量暂停 / 继续 / 删除 |
| POST | `/api/watch/progress` | 上报播放进度 |
| GET | `/api/watch/history` | 观看历史 |
| DELETE | `/api/watch/history/{id}` | 移除一条观看记录（不影响已下载文件） |
| DELETE | `/api/watch/history` | 清空观看历史 |

### 收藏 / 系统

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/favs` | 收藏列表 |
| POST | `/api/favs` | 添加收藏 |
| DELETE | `/api/favs` | 取消收藏 |
| POST | `/api/favs/seen` | 标记收藏已查看（记录当时的更新备注） |
| GET | `/api/system` | 轻量统计（兼容保留） |
| GET | `/api/sysinfo` | 运行环境、数据占用、磁盘、内存、记录数（`?fresh=1` 绕过 10s 缓存） |
| POST | `/api/system/vacuum` | 手动整理数据库 |
| GET | `/api/ffmpeg/status` | ffmpeg 状态（路径、版本、是否可下载安装） |
| POST | `/api/ffmpeg/install` | 按需下载并安装 ffmpeg |
| GET | `/api/cover/fix` | 封面缓存检查进度与统计 |
| POST | `/api/cover/fix` | 开始检查封面缓存 |
| POST | `/api/cover/fix/stop` | 停止封面检查 |
| GET | `/api/config` | 读取配置（合并 config.yml + ui.yml，含两套预设） |
| PUT | `/api/config` | 保存配置（`{"config":{...}}` 或整段原文 `{"text":"..."}`） |
| GET | `/api/version` | 当前版本 + 上次检查更新结果 |
| POST | `/api/update/check` | 检查 Gitee 是否有新版本 |
| POST | `/api/update/apply` | 下载源码并在容器内编译更新（仅 Docker） |
| GET | `/api/update/status` | 更新进度 |
| GET | `/healthz` | 健康检查（`SELECT 1`） |

## 项目结构

```
.
├── main.go                  # 启动、配置加载、目录迁移、优雅退出
├── internal/
│   ├── collector/           # 采集、封面预取/补充、下载队列
│   ├── db/                  # 建表、迁移、聚合表、分享令牌、空间整理
│   ├── downloader/          # m3u8 分片下载、ffmpeg 合并
│   ├── ffmpeg/              # ffmpeg 解析（env → data/bin → PATH）、按需下载安装、版本缓存
│   ├── imgproc/             # 封面压缩（YCbCr 平面定点缩放）
│   ├── logx/                # 结构化日志
│   ├── netguard/            # 出站 HTTP 客户端（SSRF 防护、超时）
│   ├── scheduler/           # 定时增量采集
│   ├── sysinfo/             # 磁盘/内存/目录占用（分平台实现）
│   ├── updater/             # 在线更新（下载源码、容器内编译、原子替换）
│   ├── version/             # 版本信息（VERSION + 提交 + 构建时间）
│   └── web/                 # HTTP 服务：路由 + 各业务 handler + 内嵌前端产物
│       ├── routes.go        #   路由注册（接口契约一览）
│       ├── guard.go         #   写接口守卫（管理口令 / CSRF / 请求体限额）
│       ├── playtoken.go     #   本地播放令牌
│       ├── web.go           #   Server 结构、列表缓存、中间件、通用响应
│       ├── config.go        #   配置读取 / 保存（config.yml + ui.yml）
│       ├── sources.go       #   接口管理与连通性测试
│       ├── collect.go       #   采集触发、停止、进度
│       ├── videos.go        #   影片列表 / 详情 / 筛选 / 封面代理
│       ├── favs.go          #   收藏
│       ├── downloads.go     #   下载任务、播放、分享
│       ├── watch.go         #   观看进度与历史
│       ├── system.go        #   系统信息、数据库整理、健康检查
│       ├── update.go        #   版本检查与在线更新
│       ├── live.go          #   HLS 在线播放代理（改写 m3u8 + 转发分片）
│       └── static/          #   前端构建产物（go:embed 打包）
├── web/                     # Vue 前端源码
│   ├── src/components/      #   CoverImg / Pager / ShareDialog / QrShareDialog
│   │                        #   VideoCard / VideoPlayer / ConfirmDialog
│   ├── src/views/           #   LibraryView / DetailView / LocalView / DownloadsView
│   │                        #   PlayerView / OnlineView / FavoritesView / HistoryView
│   │                        #   ShareView / SourcesView / ApiDocsView / InfoView
│   ├── src/api.ts           #   HTTP 封装（写接口自动带 JSON Content-Type）
│   ├── src/confirm.ts       #   全局确认弹窗（替代浏览器原生 confirm）
│   └── src/toast.ts         #   轻提示
├── migrations/              # 1.4.1 数据库升级 SQL（同时提供 Go 工具）
├── tools/dbupgrade/         # 独立数据库升级工具（自动备份、幂等）
├── videodata/               # 运行时数据（库、封面、下载的视频、ffmpeg 缓存）
├── bin/                     # 构建产物（不入库）
├── config.example.yml       # config.yml 精简模板
├── deploy.sh                # Linux 一键部署 / 更新脚本
├── Dockerfile
├── docker-compose.yml
└── Makefile
```

## 开发

```bash
go build ./... && go vet ./...   # 后端编译检查
gofmt -l internal main.go        # 格式检查（应为空）

make web                         # 改前端后重新构建，产物输出到 internal/web/static
make run                         # 本机跑
make run-lowmem                  # 用 NAS 上的低配参数跑
make clean                       # 只清 bin/，绝不删 videodata/
```

约定：

- 前端是 history 路由，后端 `spaHandler` 把未知路径回退到 `index.html`，新增后端路由时不要抢掉 `GET /`
- **加接口先改 `internal/web/routes.go`**，实现按域放到对应文件；写接口（POST/PUT/DELETE）
  必须用 `guard()` 包一层，否则没有管理口令 / CSRF / JSON 校验
- 修改前端后必须 `make web` 重新构建，产物已提交进仓库，否则 `go build` 拿到的是旧界面
- 项目不含自动化测试文件，验证方式：起服务后打接口（`/healthz`、`/api/sysinfo`、`/api/videos`、
  `/api/sources`）并打开 SPA 深链接（如 `/sources`、`/online/1`）

## 常见问题

**封面不显示**
页面会先显示内置占位图，说明本地还没有这张缓存。源站封面 CDN 可能有防盗链或被网络阻断，
`/api/sysinfo` 的 `cover` 字段会给出成功/失败张数和最近一次错误原因。
到「信息」页把「图片代理」填成 `https://wsrv.nl/?url=`（保存即生效，取图优先走代理），
再点「检查封面缓存」补一遍；日志会显示「取图优先走图片代理 https://wsrv.nl/?url=」。

**封面加载很慢**
这是故意的：默认每张间隔 500~1000ms，避免被图床限流。一页 100 条大约 1~2 分钟铺完，
页面会先显示占位图、下完自动补上。想调快用 `VDL_COVER_GAP_MS=300-600`。
封面检查走代理时是另一种节流：每 2~3 秒一张（`VDL_COVER_PROXY_GAP_MS` 可调，自建代理可设 `0`）。

**某个源采集一会儿就失败退出了**
日志里看「连续 3 轮抓取失败」的具体 HTTP 状态。如果是 `520`/`522`，通常是源站被 Cloudflare 拦住，
本地怎么重试都没用，等源站恢复后再点全量（会从断点续采）。

**数据库文件很大，但片库没那么多**
删接口、重新采集会留下空闲页，SQLite 不会主动归还。看 `/api/sysinfo` 的 `data.freelist_pct`，
执行一次 `VACUUM`（见上文「手动整理」）。

**下载一直卡在"合并中"**
合并由 ffmpeg 完成，弱 CPU 上很慢。日志里有合并进度；若 90 秒没有任何进度输出，看门狗会杀掉进程并把任务标为失败，可以点继续重试。

**采集点了没反应 / 一直"停止中"**
停止走的是取消 HTTP 请求上下文，一般 1 秒内生效。如果某个接口本身超时较长，等待其请求返回即可。
另外：上次已经完整跑完的接口再点全量，会从第 1 页重新开始（不是"没反应"）；中途停下的才是续采。

**在线播放提示"这条线路打不开"**
先换下一条线路。若所有线路都不行，多半是该源 CDN 从本机不可达（可先用 `curl -I <m3u8>` 验证），
或源站限制了外链播放；下载走的是同一批地址，也会一起失败。

**分享链接提示「无效或已过期」/ 本地播放 403**
分享链接按令牌的有效期判定（生成时选 24 小时、7 天或永久，上限一年），过期后重新分享即可。
本地播放要求短期播放令牌（`/api/play-token`，6 小时、进程内存态），重启或超时后刷新页面会自动重新获取；
设了 `VDL_ADMIN_TOKEN` 时也可以用管理口令直接访问。

## 说明

本项目只提供采集、存储、播放的工具能力，不托管任何影视资源。所有内容均来自用户自行添加的第三方接口，
使用时请遵守当地法律法规与接口方的服务条款，仅用于个人学习与技术研究。
