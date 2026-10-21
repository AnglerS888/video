package imgproc

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"math"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	// WebP 解码：不少图床（含 CDN 自动转码）会返回 webp，stdlib 不认识，
	// 不注册的话封面会直接"解码失败"，图片就显示不出来
	_ "golang.org/x/image/webp"
)

// 解码内存策略
//
// 封面只需要最长边 480px，但 stdlib 只能"先整张解码再缩放"：
// 一张 4000x7111 的 JPEG 解成 YCbCr 约 42MB，PNG 走 RGBA 要 113MB。
// 所以分三档：
//   - <= bigPixels：直接解，正常封面都在这一档（远小于 1MP）
//   - >  bigPixels：串行解（bigMu），并且优先交给 ffmpeg 子进程缩放 ——
//     解码在子进程里做，主进程内存不受图片尺寸影响，几千像素的长图也不会把 NAS 拖垮
//   - >  对应格式的像素上限且没有 ffmpeg：最后兜底，按硬上限拒绝（防解压炸弹）
//
// 硬上限必须按"解码后每像素占多少字节"分档，不能一个数通吃：
//   - JPEG 解出来是 YCbCr，约 1.5 字节/像素 → MaxPixels（120MP ≈ 180MB）
//   - PNG/WebP 解出来是 RGBA，4 字节/像素 → MaxPixelsRGBA（48MP ≈ 192MB）
//
// 以前两者共用 120MP，一张 120MP 的 PNG 实际要吃掉约 480MB，再叠加缩放用的
// 全尺寸副本瞬时能到 1GB，容器 limits 只有 2G 很容易被 OOM 杀掉。
// 现在 scaleGeneric 不再额外开全尺寸副本（改成按目标坐标回源图取样），
// RGBA 路径的峰值就只剩解码本身，所以阈值可以定得比 24MP 宽一些，
// 既挡住解压炸弹，又不误伤 4000x7111（28.4MP）这类正常番剧海报。
//
// 另外：以前这里是 12MP 一律拒绝，结果 4000x7111 的番剧海报（28.4MP）整张存不下来。
const (
	bigPixels = 12_000_000  // 12MP ≈ 3000x4000：超过就串行 + 优先走 ffmpeg
	MaxPixels = 120_000_000 // JPEG(YCbCr ≈ 1.5B/像素) 纯 Go 解码的硬上限 ≈ 180MB 峰值
	// MaxPixelsRGBA 是 PNG/WebP 这类走 RGBA 的格式的硬上限：4 字节/像素，
	// 48MP ≈ 192MB 解码内存（缩放缓冲已降到目标尺寸，约 1MB），
	// 超过就是坏图/解压炸弹，直接拒绝而不是把它读进内存。
	MaxPixelsRGBA = 48_000_000
	// maxPixelsFFmpeg 交给 ffmpeg 子进程的上限：子进程也吃内存（4 亿像素约 600MB），
	// 设这个纯粹是拦住明显异常的尺寸，正常海报远达不到
	maxPixelsFFmpeg = 400_000_000
)

var (
	// bigMu 大图解码串行化：并发 4 个封面 worker 各解一张 28MP 的图就是 170MB 尖峰
	bigMu sync.Mutex
	// ffmpegPath 由 main 注入（内嵌解压出来的那个），为空时大图退回纯 Go 解码。
	// 必须用原子访问：写入方是 main 的 ffmpeg 下载回调 goroutine，
	// 读取方是封面 worker goroutine；字符串是两个字长（数据指针+长度），
	// 无锁写会让读方看到"半个字符串"（撕裂读），路径直接不可用。
	ffmpegPath atomic.Pointer[string]
)

// SetFFmpegPath 设置用于超大图缩放的 ffmpeg 可执行文件；传空串表示不启用。
func SetFFmpegPath(p string) {
	if p == "" {
		// 空串用 nil 表示"未配置"，避免每个读方都要再判一次
		ffmpegPath.Store(nil)
		return
	}
	ffmpegPath.Store(&p)
}

// getFFmpegPath 原子读取当前配置的 ffmpeg 路径（空串表示未配置）。
func getFFmpegPath() string {
	if p := ffmpegPath.Load(); p != nil {
		return *p
	}
	return ""
}

// CompressImage 把封面图压成本地缓存用的小尺寸 JPEG：
//   - 等比缩放到最长边不超过 maxSide（放大不做，避免糊）
//   - 统一转 JPEG（去掉 PNG 透明通道，白底），质量 quality
//   - 返回压缩后的字节与格式名（"jpeg"）
//
// 解码失败（如 webp/avif 等 stdlib 不支持的格式）返回错误，由调用方决定是否原样缓存。
func CompressImage(raw []byte, maxSide, quality int) ([]byte, string, error) {
	if maxSide <= 0 {
		maxSide = 480
	}
	if quality <= 0 || quality > 100 {
		quality = 80
	}
	// 先只读头部尺寸：超大图在解码前就决定走哪条路
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, "", fmt.Errorf("解码失败: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, "", fmt.Errorf("图片尺寸异常")
	}
	px := int64(cfg.Width) * int64(cfg.Height)

	// 大图优先交给 ffmpeg（JPEG 没有透明通道，缩放结果与 Go 路径等价）：
	// 解码在子进程里做，主进程不会为一张 28MP 的图分配几十 MB 像素缓存。
	if px > bigPixels && (format == "jpeg" || format == "") && px <= maxPixelsFFmpeg {
		if out, ferr := scaleViaFFmpeg(raw, maxSide, quality); ferr == nil {
			return out, "jpeg", nil
		}
	}
	// 解码上限按格式分档：
	//   - JPEG 解出来是 YCbCr，约 1.5 字节/像素，120MP ≈ 180MB
	//   - PNG/WebP/GIF 解出来是 RGBA，4 字节/像素，用同一个上限等于白白多要一倍内存
	// 所以非 JPEG 走更严的 MaxPixelsRGBA，避免一张坏图把 2G 的容器撑爆。
	limit := int64(MaxPixels)
	if format != "jpeg" {
		limit = MaxPixelsRGBA
	}
	if px > limit {
		return nil, "", fmt.Errorf("图片过大 %dx%d（%.1fMP），超出解码上限", cfg.Width, cfg.Height,
			float64(cfg.Width)*float64(cfg.Height)/1e6)
	}
	if px > bigPixels {
		bigMu.Lock()
		defer bigMu.Unlock()
	}

	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, "", fmt.Errorf("解码失败: %w", err)
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return nil, "", fmt.Errorf("图片尺寸异常")
	}
	nw, nh := fitSize(w, h, maxSide)

	var out *bytes.Buffer
	// JPEG 解码出的是 YCbCr：直接在 Y/Cb/Cr 三个平面各自做双线性缩放，
	// 不转 RGBA、不逐像素调 At()（旧实现的 At() 每像素都要做一次颜色空间转换，
	// 1920x2880 一张要 39ms，弱 CPU 的 NAS 上直接是采集卡顿的来源）
	if y, ok := img.(*image.YCbCr); ok {
		dst := scaleYCbCr(y, nw, nh)
		out = encodeJPEG(dst, quality)
	} else {
		dst := scaleGeneric(img, nw, nh)
		out = encodeJPEG(dst, quality)
	}

	data := out.Bytes()
	// 极端情况下（原图本来很小）压缩反而更大，则保留原图。
	// 注意：只有"原图本来就是 JPEG"才能原样返回 —— 缓存文件名固定是 .jpg，
	// 把 webp/png 原样写进去会被按 jpeg 提供，部分浏览器会直接当坏图处理。
	if len(data) >= len(raw) && nw == w && nh == h && len(raw) < 60*1024 && isJPEG(raw) {
		return raw, "original", nil
	}
	return data, "jpeg", nil
}

// scaleViaFFmpeg 用 ffmpeg 解码并等比缩放到 maxSide，输出 JPEG 字节。
//
// 走子进程的原因：stdlib 只能整张解码，4000x7111 的 JPEG 在主进程里要约 42MB、
// PNG 更要 113MB；ffmpeg 解完立刻缩放，内存随进程释放，主进程不承担这份尖峰。
// 同一时刻只允许一个大图在跑（bigMu），避免 4 个封面 worker 同时解大图。
func scaleViaFFmpeg(raw []byte, maxSide, quality int) ([]byte, error) {
	bin := getFFmpegPath()
	if bin == "" {
		return nil, fmt.Errorf("未配置 ffmpeg")
	}
	bigMu.Lock()
	defer bigMu.Unlock()

	// quality 100 → q 2，quality 60 → q 8（ffmpeg 的 -q:v 越小越清晰）
	q := 2 + (100-quality)*15/100
	vf := fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease", maxSide, maxSide)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin,
		"-hide_banner", "-v", "error", "-i", "pipe:0",
		"-vf", vf, "-frames:v", "1", "-q:v", strconv.Itoa(q), "-f", "mjpeg", "pipe:1")
	cmd.Stdin = bytes.NewReader(raw)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := errb.String()
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return nil, fmt.Errorf("ffmpeg 缩放失败: %v %s", err, msg)
	}
	if out.Len() == 0 {
		return nil, fmt.Errorf("ffmpeg 输出为空")
	}
	return out.Bytes(), nil
}

// isJPEG 按魔数判断（SOI + 后续标记）
func isJPEG(b []byte) bool {
	return len(b) > 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF
}

// fitSize 按最长边等比缩放（只缩不放）
func fitSize(w, h, maxSide int) (int, int) {
	if w <= maxSide && h <= maxSide {
		return w, h
	}
	nw, nh := w, h
	if w >= h {
		nw = maxSide
		nh = int(math.Round(float64(h) * float64(maxSide) / float64(w)))
	} else {
		nh = maxSide
		nw = int(math.Round(float64(w) * float64(maxSide) / float64(h)))
	}
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	return nw, nh
}

func encodeJPEG(img image.Image, quality int) *bytes.Buffer {
	var buf bytes.Buffer
	// 预估容量：480x720 的 JPEG 大约 20~60KB，避免多次扩容
	buf.Grow(64 << 10)
	_ = jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality})
	return &buf
}

// chromaDim 按采样比算出色度平面尺寸（与 image.YCbCr 的 CStride/高度规则一致）
func chromaDim(w, h int, r image.YCbCrSubsampleRatio) (cw, ch int, hSub, vSub int) {
	switch r {
	case image.YCbCrSubsampleRatio422:
		return (w + 1) / 2, h, 2, 1
	case image.YCbCrSubsampleRatio420:
		return (w + 1) / 2, (h + 1) / 2, 2, 2
	case image.YCbCrSubsampleRatio440:
		return w, (h + 1) / 2, 1, 2
	case image.YCbCrSubsampleRatio411:
		return (w + 3) / 4, h, 4, 1
	case image.YCbCrSubsampleRatio410:
		return (w + 3) / 4, (h + 1) / 2, 4, 2
	}
	return w, h, 1, 1
}

// scaleYCbCr 在 YCbCr 空间内缩放（三个平面各做一次双线性），
// 输出直接可交给 jpeg.Encode（JPEG 原生就是 YCbCr，编码时不再做颜色转换）。
func scaleYCbCr(src *image.YCbCr, nw, nh int) *image.YCbCr {
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	dst := image.NewYCbCr(image.Rect(0, 0, nw, nh), src.SubsampleRatio)
	scw, sch, _, _ := chromaDim(sw, sh, src.SubsampleRatio)
	cw, ch, _, _ := chromaDim(nw, nh, src.SubsampleRatio)

	scalePlane(dst.Y, nw, nw, nh, src.Y, src.YStride, sb.Min.X, sb.Min.Y, sw, sh)
	scalePlane(dst.Cb, dst.CStride, cw, ch, src.Cb, src.CStride, sb.Min.X/2, sb.Min.Y/2, scw, sch)
	scalePlane(dst.Cr, dst.CStride, cw, ch, src.Cr, src.CStride, sb.Min.X/2, sb.Min.Y/2, scw, sch)
	return dst
}

// scalePlane 把单个平面双线性缩放到 dw x dh。
//
//	src 源平面，srcStride 源行跨度，srcOffX/srcOffY 平面内起始坐标，
//	sw/sh 源平面尺寸，dst 目标平面（行跨度 dstStride）
//
// 全部用整数定点运算（8 位小数权重），避免每像素浮点除法——弱 CPU（J4125/N5105 一类）
// 上这一步的差距就是"采集时 CPU 打满"与"后台平稳"的差别。
func scalePlane(dst []byte, dstStride int, dw, dh int, src []byte, srcStride, srcOffX, srcOffY, sw, sh int) {
	if sw <= 0 || sh <= 0 || dw <= 0 || dh <= 0 {
		return
	}
	if sw == dw && sh == dh {
		for y := 0; y < dh; y++ {
			so := (srcOffY+y)*srcStride + srcOffX
			copy(dst[y*dstStride:y*dstStride+dw], src[so:so+dw])
		}
		return
	}

	const one = 1 << 8 // 权重精度
	x0 := make([]int, dw)
	x1 := make([]int, dw)
	fx := make([]int32, dw)
	for x := 0; x < dw; x++ {
		// 目标像素中心映射回源坐标（放大/缩小都用同一公式）。
		// 这里必须用 int64：32 位构建（例如 GoLand 默认的 GOARCH=386）下
		// (2*x+1)*sw*256 在 4000 像素宽的图上就会超过 int32 范围，溢出后索引错乱甚至 panic。
		pos := int((int64(2*x+1) * int64(sw) * one) / int64(2*dw))
		pos -= one / 2
		if pos < 0 {
			pos = 0
		}
		maxPos := (sw - 1) * one
		if pos > maxPos {
			pos = maxPos
		}
		xi := pos >> 8
		x1v := xi + 1
		if x1v >= sw {
			x1v = xi
		}
		x0[x] = srcOffX + xi
		x1[x] = srcOffX + x1v
		fx[x] = int32(pos & (one - 1))
	}

	for y := 0; y < dh; y++ {
		pos := int((int64(2*y+1) * int64(sh) * one) / int64(2*dh))
		pos -= one / 2
		if pos < 0 {
			pos = 0
		}
		maxPos := (sh - 1) * one
		if pos > maxPos {
			pos = maxPos
		}
		yi := pos >> 8
		fy := int32(pos & (one - 1))
		y1 := yi + 1
		if y1 >= sh {
			y1 = yi
		}
		r0 := (srcOffY+yi)*srcStride + 0
		r1 := (srcOffY+y1)*srcStride + 0
		dr := dst[y*dstStride : y*dstStride+dw]
		for x := 0; x < dw; x++ {
			a := int32(src[r0+x0[x]])
			b := int32(src[r0+x1[x]])
			c := int32(src[r1+x0[x]])
			d := int32(src[r1+x1[x]])
			wx := fx[x]
			top := a*(one-wx) + b*wx
			bot := c*(one-wx) + d*wx
			dr[x] = byte((top*(one-fy) + bot*fy) >> 16)
		}
	}
}

// scaleGeneric 非 JPEG（PNG/WebP/GIF 等）走通用路径：
// 直接在源图上按目标像素坐标取样，双线性缩放后输出目标尺寸的 RGBA（白底合成）。
//
// 为什么不再"先拷一份全尺寸 RGBA，再按通道分别缩放"：
//   - 全尺寸副本是 4 字节/像素，24MP 的 PNG 光这份副本就要 96MB（解码那份已经 96MB），
//     几个封面 worker 并发时很容易把 2G 的容器顶到 OOM；现在内存里只剩目标尺寸的一份缓冲。
//   - 那个版本的写入端按通道下标 ch 取值、读取端却恒取 +0（R 通道），
//     等于把 R 值分别写进 G/B/A：彩色封面被压成灰度，alpha 也被写坏。
//     现在一次采样同时取 RGB 三通道，读写用的是同一份数据，从根上杜绝通道错位。
func scaleGeneric(src image.Image, nw, nh int) *image.RGBA {
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return dst
	}
	if w == nw && h == nh {
		// 尺寸不变：白底合成一次即可（PNG 透明区域不会变黑）
		draw.Draw(dst, dst.Bounds(), &image.Uniform{color.White}, image.Point{}, draw.Src)
		draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Over)
		return dst
	}

	s := newSrcSampler(src, b)

	const one = 1 << 8 // 权重精度：8 位小数
	x0 := make([]int, nw)
	x1 := make([]int, nw)
	fx := make([]int32, nw)
	for x := 0; x < nw; x++ {
		// 目标像素中心映射回源坐标（放大/缩小都用同一公式）。
		// 必须用 int64：32 位构建（GOARCH=386）下 (2*x+1)*w*256
		// 在几千像素宽的图上就会超过 int32 范围，溢出后索引错乱甚至 panic。
		pos := int((int64(2*x+1) * int64(w) * one) / int64(2*nw))
		pos -= one / 2
		if pos < 0 {
			pos = 0
		}
		if maxPos := (w - 1) * one; pos > maxPos {
			pos = maxPos
		}
		xi := pos >> 8
		xi1 := xi + 1
		if xi1 >= w {
			xi1 = xi
		}
		x0[x] = xi
		x1[x] = xi1
		fx[x] = int32(pos & (one - 1))
	}

	for y := 0; y < nh; y++ {
		pos := int((int64(2*y+1) * int64(h) * one) / int64(2*nh))
		pos -= one / 2
		if pos < 0 {
			pos = 0
		}
		if maxPos := (h - 1) * one; pos > maxPos {
			pos = maxPos
		}
		yi := pos >> 8
		fy := int32(pos & (one - 1))
		y1 := yi + 1
		if y1 >= h {
			y1 = yi
		}
		row := dst.Pix[y*dst.Stride : y*dst.Stride+nw*4]
		for x := 0; x < nw; x++ {
			r00, g00, b00 := s.at(x0[x], yi)
			r01, g01, b01 := s.at(x1[x], yi)
			r10, g10, b10 := s.at(x0[x], y1)
			r11, g11, b11 := s.at(x1[x], y1)
			wx := fx[x]
			// 定点双线性：权重和恒为 one，中间值 ≤ 65280，累加后仍在 int32 范围内
			rt := (int32(r00)*(one-wx)+int32(r01)*wx)*(one-fy) + (int32(r10)*(one-wx)+int32(r11)*wx)*fy
			gt := (int32(g00)*(one-wx)+int32(g01)*wx)*(one-fy) + (int32(g10)*(one-wx)+int32(g11)*wx)*fy
			bt := (int32(b00)*(one-wx)+int32(b01)*wx)*(one-fy) + (int32(b10)*(one-wx)+int32(b11)*wx)*fy
			i := x * 4
			row[i] = byte(rt >> 16)
			row[i+1] = byte(gt >> 16)
			row[i+2] = byte(bt >> 16)
			row[i+3] = 255 // 已与白底合成，输出不透明
		}
	}
	return dst
}

// 采样器类型：决定 at() 走哪条快路径
const (
	sampOther = iota
	sampRGBA
	sampNRGBA
	sampPaletted
	sampGray
)

// srcSampler 按坐标从源图取样，并统一做"白底合成"。
//
// 合成语义与旧的 draw.Over 到白底完全一致：半透明像素按 alpha 混到白底上，
// 不会因为透明像素自身 RGB 为 0 而把封面压成黑底。
type srcSampler struct {
	kind int
	img  image.Image
	pal  []color.RGBA // Paletted 专用查表：调色板下标 → 白底合成后的 RGB
	offX int
	offY int
}

// newSrcSampler 按源图类型挑一条最省的取样路径。
func newSrcSampler(src image.Image, b image.Rectangle) *srcSampler {
	s := &srcSampler{img: src, offX: b.Min.X, offY: b.Min.Y}
	switch v := src.(type) {
	case *image.RGBA:
		s.kind = sampRGBA
	case *image.NRGBA:
		s.kind = sampNRGBA
	case *image.Paletted:
		s.kind = sampPaletted
		// GIF 都是几百色调色板，预先算好白底合成结果，
		// 比每个采样点都走一次 At()+颜色转换快一个量级
		s.pal = make([]color.RGBA, len(v.Palette))
		for i, c := range v.Palette {
			cr, cg, cb, ca := c.RGBA()
			s.pal[i] = color.RGBA{
				R: clamp8((int(cr) + 65535 - int(ca)) >> 8),
				G: clamp8((int(cg) + 65535 - int(ca)) >> 8),
				B: clamp8((int(cb) + 65535 - int(ca)) >> 8),
				A: 255,
			}
		}
	case *image.Gray:
		// 灰度图没有透明通道，采样就是取一个灰度值填三通道
		s.kind = sampGray
	default:
		s.kind = sampOther
	}
	return s
}

// at 返回 (x,y) 处与白底合成后的 RGB（8 位）。
// x,y 是相对于源图左上角的偏移（0 起）。
func (s *srcSampler) at(x, y int) (uint8, uint8, uint8) {
	switch s.kind {
	case sampRGBA:
		// image.RGBA 是预乘 alpha：合成结果 = 预乘值 + (255-alpha)
		p := s.img.(*image.RGBA)
		i := p.PixOffset(s.offX+x, s.offY+y)
		q := p.Pix[i : i+4 : i+4]
		inv := 255 - int(q[3])
		return clamp8(int(q[0]) + inv), clamp8(int(q[1]) + inv), clamp8(int(q[2]) + inv)
	case sampNRGBA:
		// image.NRGBA 存的是非预乘值：先按 alpha 预乘再叠白底
		p := s.img.(*image.NRGBA)
		i := p.PixOffset(s.offX+x, s.offY+y)
		q := p.Pix[i : i+4 : i+4]
		a := int(q[3])
		inv := 255 - a
		return clamp8(int(q[0])*a/255 + inv), clamp8(int(q[1])*a/255 + inv), clamp8(int(q[2])*a/255 + inv)
	case sampPaletted:
		p := s.img.(*image.Paletted)
		idx := int(p.Pix[p.PixOffset(s.offX+x, s.offY+y)])
		if idx < 0 || idx >= len(s.pal) {
			// 越界调色板下标（损坏的 GIF）：当作透明 → 白底
			return 255, 255, 255
		}
		c := s.pal[idx]
		return c.R, c.G, c.B
	case sampGray:
		// 灰度图不透明，直接把灰度值填到三个通道
		p := s.img.(*image.Gray)
		v := p.GrayAt(s.offX+x, s.offY+y).Y
		return v, v, v
	default:
		// 其它类型（CMYK/NYCbCr 等）：At().RGBA() 返回 16 位预乘分量，
		// 合成到白底 = 分量 + 65535 - alpha
		cr, cg, cb, ca := s.img.At(s.offX+x, s.offY+y).RGBA()
		return clamp8((int(cr) + 65535 - int(ca)) >> 8),
			clamp8((int(cg) + 65535 - int(ca)) >> 8),
			clamp8((int(cb) + 65535 - int(ca)) >> 8)
	}
}

// clamp8 把结果夹到 0..255（防止异常图片导致字节溢出回绕）
func clamp8(v int) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}
