#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
adscan —— HLS 贴片广告段检测器（纯标准库，不依赖 ffmpeg / numpy）

用法:
    python adscan.py <m3u8地址> [--workers 16] [--probe 262144] [--json out.json]
                     [--fast] [--all]

原理:
    采集站/资源站把广告用**另一条编码流**独立封装后拼进正片，
    拼接处必然留下编码层/封装层的系统性差异。本工具在 4 个互相独立的
    维度上找「少数派异常」，用投票+并集给出广告段：

    A. 切片层  EXTINF 时长偏离全局众数（重新切片的痕迹）
    B. 码流层  PTS 绝对值量级跳变（广告是独立时间轴）
    C. 编码层  SPS/VUI 参数不同（fps、色度原色、分辨率）
                + SPS 原始字节指纹不同
                + x264 编码器版本（SEI user_data_unregistered）
    D. 封装层  PMT 段指纹不同（广告常带自己的 registration descriptor / 广告商标识）

    单条判据都可能有假阳性，故采用「硬判据命中即算 / 软判据需 2 条以上」策略。

    ⚠️ 段号不能跨批次复用：同一部剧的 m3u8 会随签名批次（约 30 分钟）重新切片，
       段文件名/数量都会变。对外只应给出「播放时间区间」。
"""

import sys
import os
import re
import json
import hashlib
import statistics
import urllib.request
import urllib.error
from collections import Counter
from concurrent.futures import ThreadPoolExecutor
from urllib.parse import urljoin, urlparse

sys.stdout.reconfigure(encoding="utf-8")

UA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/120 Safari/537.36"
TICK = 90000.0

# ============================================================ 站点级已知广告指纹库
#
# 与「正片 SPS 指纹」相反的一类常量：
#   正片 SPS 指纹随母版变（每部剧都不同）→ 不能硬编码；
#   **广告 SPS 指纹随插入器/广告库变（同一站点固定）→ 可以硬编码**。
#
# 实测：wsyzym3u8 站两部完全不同的剧（202609/30/D0B1D0qnTG27 与 202610/25/MEt06UAxyG27，
# 正片母版不同），其广告段的 SPS 指纹**逐字节一致**，段内位置结构也一一对应
# （首段 b9a628…，中段 ad2be41…）。→ 同一套广告素材 + 同一条编码管线。
#
# 命中即几乎可断定是广告，作为硬判据参与投票。
# 发现新的广告指纹时，往对应域名的集合里加一行即可。
KNOWN_AD_SPS = {
    "wsyzym3u8.com": {
        "b9a628441195b186ed75d36e0a10bfbb",   # 广告首段 SPS
        "ad2be416397d9a26005ca389cfac44b6",   # 广告中段 SPS
    },
    "ffzy-plays.com": {
        "1c42ccb927e09a7a3c46461e4410d737",   # 广告段 SPS（该源 5 段里仅此段带 in-band SPS）
    },
}


def known_ad_fps(media_url):
    """取出该源所属站点的已知广告 SPS 指纹集合；无则返回空集。"""
    host = (urlparse(media_url).hostname or "").lower()
    out = set()
    for dom, fps in KNOWN_AD_SPS.items():
        if host == dom or host.endswith("." + dom):
            out |= fps
    return out


# ============================================================ HTTP

def http_get(url, rng=None, timeout=25):
    req = urllib.request.Request(url, headers={"User-Agent": UA})
    if rng:
        req.add_header("Range", "bytes=%d-%d" % rng)
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return r.read()


# ============================================================ m3u8

def load_playlist(url, depth=0):
    """自动从 master 下钻到 media playlist。返回 (media_url, lines)。"""
    if depth > 3:
        return url, []
    txt = http_get(url).decode("utf-8", "replace")
    lines = [l.strip() for l in txt.splitlines() if l.strip()]
    if any(l.startswith("#EXT-X-STREAM-INF") for l in lines):
        cands = []
        for i, l in enumerate(lines):
            if l.startswith("#EXT-X-STREAM-INF"):
                bw = re.search(r"BANDWIDTH=(\d+)", l)
                res = re.search(r"RESOLUTION=(\d+x\d+)", l)
                for j in range(i + 1, len(lines)):
                    if not lines[j].startswith("#"):
                        cands.append((int(bw.group(1)) if bw else 0,
                                      res.group(1) if res else "", lines[j]))
                        break
        if cands:
            # 优先分辨率最高，其次码率最高
            cands.sort(key=lambda c: (c[1], c[0]))
            best = cands[-1]
            child = urljoin(url, best[2])
            return load_playlist(child, depth + 1)
    return url, lines


def parse_segments(lines, base=""):
    segs, cur, disc = [], None, False
    for l in lines:
        if l.startswith("#EXTINF"):
            try:
                dur = float(l.split(":")[1].split(",")[0])
            except Exception:
                dur = 0.0
            cur = {"dur": dur, "disc": disc}
            disc = False
        elif l.startswith("#EXT-X-DISCONTINUITY"):
            disc = True
        elif not l.startswith("#") and cur is not None:
            cur["url"] = urljoin(base, l) if base else l
            segs.append(cur)
            cur = None
    t = 0.0
    for s in segs:
        s["start"] = t
        t += s["dur"]
        s["end"] = t
    return segs, t


# ============================================================ H.264 / SPS

class BR:
    def __init__(self, d):
        self.d, self.p = d, 0

    def u(self, n):
        v = 0
        for _ in range(n):
            v = (v << 1) | ((self.d[self.p >> 3] >> (7 - (self.p & 7))) & 1)
            self.p += 1
        return v

    def ue(self):
        z = 0
        while self.u(1) == 0:
            z += 1
            if z > 32:
                return 0
        return (1 << z) - 1 + (self.u(z) if z else 0)

    def se(self):
        v = self.ue()
        return (v + 1) // 2 if v & 1 else -(v // 2)


def rbsp(b):
    out, i = bytearray(), 0
    while i < len(b):
        if i + 2 < len(b) and b[i] == 0 and b[i + 1] == 0 and b[i + 2] == 3:
            out += b[i:i + 2]
            i += 3
        else:
            out.append(b[i])
            i += 1
    return bytes(out)


CP = {1: "bt709", 2: "undef", 4: "bt470m", 5: "bt470bg", 6: "smpte170m", 9: "bt2020"}
TC = {1: "bt709", 2: "undef", 4: "gamma22", 5: "gamma28", 8: "linear", 13: "iec61966"}
MC = {0: "rgb", 1: "bt709", 2: "undef", 4: "bt470m", 5: "bt470bg",
      6: "smpte170m", 7: "bt2020nc", 9: "bt2020c"}
HI = {100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135}


def parse_sps(nal):
    """解析 SPS，返回 {profile,level,w,h,fps,cp,tc,mc,full_range,vui}。"""
    r = BR(rbsp(nal[1:]))
    out = {}
    out["profile"] = r.u(8)
    r.u(8)
    out["level"] = r.u(8)
    r.ue()
    chroma = 1
    if out["profile"] in HI:
        chroma = r.ue()
        if chroma == 3:
            r.u(1)
        r.ue(); r.ue(); r.u(1)
        if r.u(1):                      # scaling matrix（本工具不支持，放弃该 SPS）
            raise ValueError("scaling_matrix")
    r.ue()                              # log2_max_frame_num_minus4
    poc = r.ue()
    if poc == 0:
        r.ue()
    elif poc == 1:
        r.u(1); r.se(); r.se()
        for _ in range(r.ue()):
            r.se()
    r.ue()                              # max_num_ref_frames
    r.u(1)                              # gaps_in_frame_num
    w, h = r.ue(), r.ue()
    fmo = r.u(1)
    if not fmo:
        r.u(1)
    r.u(1)                              # direct_8x8
    crop = None
    if r.u(1):
        crop = (r.ue(), r.ue(), r.ue(), r.ue())
    unitX = 2 if chroma in (1, 2) else 1
    unitY = 2 if chroma == 1 else 1
    out["w"] = (w + 1) * 16 - (unitX * (crop[0] + crop[1]) if crop else 0)
    out["h"] = ((h + 1) * 16 - (unitY * (crop[2] + crop[3]) if crop else 0)) * (2 if fmo else 1)
    out["fps"] = None
    out["cp"] = out["tc"] = out["mc"] = None
    out["full_range"] = None
    out["vui"] = bool(r.u(1))
    if out["vui"]:
        if r.u(1):                      # aspect_ratio_info
            idc = r.u(8)
            if idc == 255:
                r.ue(); r.ue()
        if r.u(1):                      # overscan
            r.u(1)
        if r.u(1):                      # video_signal_type
            r.u(3)
            out["full_range"] = r.u(1)
            if r.u(1):                  # colour_description_present
                out["cp"] = CP.get(r.u(8), "?")
                out["tc"] = TC.get(r.u(8), "?")
                out["mc"] = MC.get(r.u(8), "?")
        if r.u(1):                      # chroma_loc_info
            r.u(8)
        if r.u(1):                      # timing_info
            num, scale = r.u(32), r.u(32)
            r.u(1)
            if num:
                out["fps"] = round(scale / (2.0 * num), 3)
    return out


def sei_user_data(nals):
    """抽取 SEI user_data_unregistered（x264 编码器版本串等）。"""
    out = []
    for n in nals:
        if not n or (n[0] & 0x1F) != 6:
            continue
        b = rbsp(n[1:])
        i = 0
        try:
            while i < len(b):
                t = 0
                while i < len(b):
                    x = b[i]; i += 1; t += x
                    if x != 0xFF:
                        break
                if t == 0:
                    break
                if t == 5:
                    i += 16
                    if i < len(b):
                        ln = b[i]; i += 1
                        # SEI 载荷前面常带 UUID 尾部 / country_code 等控制字节，
                        # 直接 latin1 解码会印出乱码（如 "îï"）。只保留可打印 ASCII。
                        s = b[i:i + ln].decode("latin1", "replace")
                        s = "".join(c for c in s if 32 <= ord(c) < 127).strip()
                        if s:
                            out.append(s)
                        i += ln
                elif t == 1:
                    i += 1
                else:
                    if i < len(b):
                        ln = b[i]; i += 1 + ln
        except Exception:
            pass
    return out


def split_annexb(buf):
    starts = []
    j, n = 0, len(buf)
    while j < n - 3:
        if buf[j] == 0 and buf[j + 1] == 0 and buf[j + 2] == 1:
            starts.append(j + 3)
            j += 3
        else:
            j += 1
    out = []
    for k, s in enumerate(starts):
        e = starts[k + 1] - 3 if k + 1 < len(starts) else n
        out.append(buf[s:e])
    return out


# ============================================================ TS 解析

def parse_ts(data):
    """解析 TS 字节流，提取 PMT/PES-PTS/H.264 参数。

    ⚠️ 不依赖 PAT：实测部分采集源的 PAT 里 program_map_PID 字段是坏的
       （本例指向 256，但真正的 PMT 在 4096），按 PAT 找会全盘失败。
       改为直接扫 table_id=0x02 找 PMT，并用 PES stream_id 识别视频/音频流。
    """
    vpid = apid = None
    pmt_pid = None
    streams = {}
    pts = None
    es = []
    pmt_fp = None
    pmt_txt = ""

    n = len(data) // 188
    for k in range(n):
        p = data[k * 188:(k + 1) * 188]
        if len(p) < 188 or p[0] != 0x47:
            continue
        pid = ((p[1] & 0x1F) << 8) | p[2]
        pusi = bool(p[1] & 0x40)
        afc = (p[3] >> 4) & 0x3
        off = 4
        if afc & 2:                      # adaptation field
            if p[4] >= 183:
                continue
            off += 1 + p[4]
        if off >= 188 or not (afc & 1):
            continue
        pl = p[off:]
        if not pl:
            continue

        # ---- PMT：table_id = 0x02 ----
        if pusi and pmt_fp is None and pid != 0:
            sec = pl[1 + pl[0]:]
            if len(sec) >= 16 and sec[0] == 0x02:
                sl = ((sec[1] & 0x0F) << 8) | sec[2]
                if 12 <= sl <= 1024:
                    body = sec[3:3 + sl - 4]
                    if len(body) == sl - 4 and len(body) >= 9:
                        pmt_pid = pid
                        pmt_fp = body.hex()
                        pmt_txt = "".join(chr(c) if 32 <= c < 127 else "." for c in body)
                        i = 9 + (((body[7] & 0x0F) << 8) | body[8])
                        while i + 5 <= len(body):
                            st = body[i]
                            ep = ((body[i + 1] & 0x1F) << 8) | body[i + 2]
                            streams[ep] = st
                            i += 5 + (((body[i + 3] & 0x0F) << 8) | body[i + 4])
                        continue

        # ---- PES 识别流类型（PAT/PMT 不可靠时的兜底）----
        if pusi and len(pl) >= 9 and pl[:3] == b"\x00\x00\x01":
            sid = pl[3]
            if sid in (0xE0, 0xE1, 0x20, 0x21, 0x40, 0xFD) and vpid is None:
                vpid = pid
            elif sid in (0xC0, 0xC1, 0xBD) and apid is None:
                apid = pid

        # ---- 收集视频 ES ----
        # 注意：某些流在视频轨里嵌了 CEA-608 明文字幕（如 "lock=1:0:0 ..."），
        # 会污染整个 ES 字节流里的 start-code 扫描。必须在**每个 PES 单元内**
        # 独立切 NAL，而不是把整段 ES 拼起来再切。
        if vpid is not None and pid == vpid:
            if pusi and len(pl) >= 9 and pl[:3] == b"\x00\x00\x01":
                if pl[7] & 0x80 and len(pl) >= 14:
                    t = 0
                    for b in pl[9:14]:
                        t = (t << 8) | b
                    if pts is None:
                        pts = ((t >> 33) & 0x7) << 30 | ((t >> 1) & 0x7FFFFFFF)
                es.append(pl[9 + pl[8]:])      # list of PES payload chunks
            else:
                es.append(pl)

    # SPS / SEI：逐 PES 单元独立切 NAL（避免字幕文本污染 start-code 扫描）
    sps = None
    sps_md5 = None
    sei = []
    for chunk in es:
        for n_ in split_annexb(chunk):
            if not n_:
                continue
            t = n_[0] & 0x1F
            if t == 7 and sps is None:
                try:
                    sps = parse_sps(n_)
                except Exception:
                    sps = None
                sps_md5 = hashlib.md5(n_).hexdigest()
            elif t == 6 and not sei:
                sei = sei_user_data([n_])
        if sps_md5 and sei:
            break

    return {
        "pmt_pid": pmt_pid, "vpid": vpid, "apid": apid,
        "pts": pts, "sps": sps, "sps_md5": sps_md5,
        "sei": sei, "pmt_fp": pmt_fp, "pmt_txt": pmt_txt,
    }


# ============================================================ 特征提取

def probe_segment(url, probe_bytes):
    """分级取段首字节并解析：逐级放大窗口，直到拿到 SPS + PMT。

    窗口太小会漏掉 SPS（广告段 SPS 常落在段首 128KB 之外，或不在首个 PES 单元里），
    这会把正常段误判成「缺少色度标记」。所以必须放大窗口重取，而不是放宽判据。
    """
    r = {"err": "未取到数据"}
    for size in (probe_bytes, probe_bytes * 3, probe_bytes * 9, 0):
        try:
            if size:
                data = http_get(url, (0, size - 1))
            else:
                data = http_get(url)
        except Exception as e:
            r = {"err": str(e)}
            continue
        if not data:
            continue
        try:
            r = parse_ts(data)
        except Exception as e:
            r = {"err": "parse: %s" % e}
            continue
        # 拿到 SPS + PMT 才算够用（这是判定所需的最少信息）
        if r.get("sps_md5") and r.get("pmt_fp"):
            return r
        if size == 0:
            return r
    return r


def scan(url, workers=16, probe=128 * 1024):
    media_url, lines = load_playlist(url)
    segs, total = parse_segments(lines, media_url)
    n = len(segs)
    print("媒体播放列表: %s" % media_url)
    print("段数 %d, 总时长 %.2fs (%.2f min)" % (n, total, total / 60))
    if n == 0:
        return {}

    # ---------- A. 切片层：EXTINF 众数 ----------
    durs = [s["dur"] for s in segs]
    mode = statistics.mode([round(d, 3) for d in durs])
    anom = [i for i, s in enumerate(segs) if abs(s["dur"] - mode) > 0.02]
    ratio = len(anom) / n
    usable = ratio <= 0.2
    print("众数时长 %.3fs, 偏离段 %d/%d (%.0f%%) → 时长判据%s"
          % (mode, len(anom), n, ratio * 100, "可用" if usable else "不适用(变长切片源)"))
    if usable and anom:
        print("  偏离段: %s" % anom[:40])

    # --fast：只读 playlist，零下载初筛
    if "--fast" in sys.argv:
        blocks = []
        if usable:
            for i in anom:
                if blocks and i == blocks[-1][-1] + 1:
                    blocks[-1].append(i)
                else:
                    blocks.append([i])
        print("\n" + "=" * 66)
        print("[fast 模式] 仅凭 playlist 的候选 (零下载)")
        print("=" * 66)
        for blk in blocks:
            a, b = blk[0], blk[-1]
            print("▸ %s ~ %s  (段 #%d~#%d, %.2fs)"
                  % (hms(segs[a]["start"]), hms(segs[b]["end"]), a, b,
                     segs[b]["end"] - segs[a]["start"]))
        if not blocks:
            print("未发现时长异常段 —— 该判据在此源上不可用（变长切片）")
        return dict(segs=segs, info={}, cuts=[], blocks=blocks,
                    host_md5=None, main_pmt=None, main_enc=None,
                    main_fps=None, main_cp=None, mode=mode)

    # ---------- 探测每段 ----------
    info = {}

    def one(i):
        try:
            return i, probe_segment(segs[i]["url"], probe)
        except Exception as e:
            return i, {"err": str(e)}

    with ThreadPoolExecutor(max_workers=workers) as ex:
        for i, r in ex.map(one, range(n)):
            info[i] = r
            if (i + 1) % 100 == 0:
                print("  探测 %d/%d" % (i + 1, n))
    print("探测完成")

    # ---------- B. 码流层：PTS 量级跳变 ----------
    pts = [info[i].get("pts") for i in range(n)]
    cuts = []
    for a in range(1, n):
        pa, pb = pts[a - 1], pts[a]
        if pa is None or pb is None:
            continue
        d = (pb - pa) % (1 << 33)
        # 正常步进应与时长同量级；偏离 30s 以上视为量级跳变
        if abs(d) > 30 * TICK:
            cuts.append((a, d / TICK))
    print("PTS 量级跳变 %d 处: %s" % (len(cuts), [(a, round(x, 1)) for a, x in cuts][:20]))

    # ---------- C. 编码层：SPS 指纹 / SPS 参数 / SEI ----------
    md5s = Counter(info[i]["sps_md5"] for i in range(n) if info[i].get("sps_md5"))
    host_md5 = md5s.most_common(1)[0][0] if md5s else None
    foreign = [i for i in range(n)
               if info[i].get("sps_md5") and host_md5 and info[i]["sps_md5"] != host_md5]
    print("正片 SPS 指纹 %s (占 %d/%d 带 SPS 段)"
          % (host_md5, md5s.most_common(1)[0][1] if md5s else 0, sum(md5s.values())))

    def vote(key):
        c = Counter(info[i]["sps"].get(key) for i in range(n)
                    if info[i].get("sps") and info[i]["sps"].get(key) is not None)
        return c.most_common(1)[0][0] if c else None

    main_fps = vote("fps")
    main_cp = vote("cp")
    main_res = vote("w")
    main_mc = vote("mc")
    print("正片 SPS 主流: fps=%s 色度原色=%s 矩阵=%s 宽=%s" % (main_fps, main_cp, main_mc, main_res))

    # ---------- D. 封装层：PMT 指纹（基线提前算，供编码器基线筛选用）----------
    pmts = Counter(info[i]["pmt_fp"] for i in range(n) if info[i].get("pmt_fp"))
    main_pmt = pmts.most_common(1)[0][0] if pmts else None

    # 编码器版本：基线**只取正片段**（广告段的编码器本就该不同，
    # 混在一起统计会让众数落到广告上 → 反而把正片全判成广告）。
    encs = Counter()
    for i in range(n):
        if host_md5 and info[i].get("sps_md5") and info[i]["sps_md5"] != host_md5:
            continue
        if main_pmt and info[i].get("pmt_fp") and info[i]["pmt_fp"] != main_pmt:
            continue
        for s in info[i].get("sei") or []:
            if "x264" in s or "x265" in s:
                encs[s] += 1
                break
    main_enc = encs.most_common(1)[0][0] if encs else None
    print("正片编码器: %s (正片样本 %d 段%s)"
          % (main_enc or "未取到", sum(encs.values()),
             ", 备选 %s" % dict(encs) if len(encs) > 1 else ""))

    # ---------- D. 封装层：PMT 指纹 ----------
    print("正片 PMT 指纹: %d/%d 段一致" % (pmts.most_common(1)[0][1] if pmts else 0,
                                           sum(pmts.values())))
    for fp, c in pmts.most_common():
        if fp != main_pmt:
            idx = [i for i in range(n) if info[i].get("pmt_fp") == fp]
            print("  异源 PMT ×%d → 段 %s  标识=%r"
                  % (c, idx[:20], info[idx[0]].get("pmt_txt", "")[-40:] if idx else ""))

    # ---------- 判定 ----------
    hard_map = {}
    soft_map = {}
    known_ads = known_ad_fps(media_url)
    if known_ads:
        print("站点已知广告指纹 %d 条（命中即判）" % len(known_ads))
    for i in range(n):
        r = info[i]
        hard, soft = [], []
        sps = r.get("sps") or {}

        # 站点级已知广告指纹：同站广告库编码管线固定，命中几乎可断定
        if known_ads and r.get("sps_md5") in known_ads:
            hard.append("命中已知广告指纹")
        if host_md5 and r.get("sps_md5") and r["sps_md5"] != host_md5:
            hard.append("SPS 指纹不同")
        if main_fps and sps.get("fps") and sps["fps"] != main_fps:
            hard.append("fps=%s(正片=%s)" % (sps["fps"], main_fps))
        # 只有确实解析出了 SPS，才能拿它的色度字段做判据
        # （没抓到 SPS ≠ SPS 里没有色度标记，前者是探测不足，后者才是真差异）
        if sps:
            if main_cp and sps.get("cp") is None:
                hard.append("缺少色度原色标记(正片=%s)" % main_cp)
            elif main_cp and sps.get("cp") and sps["cp"] != main_cp:
                hard.append("色度=%s(正片=%s)" % (sps["cp"], main_cp))
            if main_mc and sps.get("mc") and sps["mc"] != main_mc:
                hard.append("矩阵=%s(正片=%s)" % (sps["mc"], main_mc))
            if main_res and sps.get("w") and sps["w"] != main_res:
                hard.append("宽度=%s(正片=%s)" % (sps["w"], main_res))
        if main_enc:
            for s in r.get("sei") or []:
                if "x264" in s or "x265" in s:
                    if s != main_enc:
                        hard.append("编码器=%s" % s)
                    break
        if main_pmt and r.get("pmt_fp") and r["pmt_fp"] != main_pmt:
            hard.append("PMT 指纹不同")
        # 软判据
        # 注意：「段内无 SPS」不能当判据 —— 探测窗口不足也会导致取不到 SPS，
        # 会把正常段误判成广告。这里只在已确认拿到过 SPS 的对照下才有意义，故不采用。
        if r.get("pts") is not None and any(a == i for a, _ in cuts):
            soft.append("PTS 量级跳变点")
        if usable and i in anom:
            soft.append("时长偏离众数(%.3f vs %.3f)" % (segs[i]["dur"], mode))

        hard_map[i] = hard
        soft_map[i] = soft

    suspect = []
    for i in range(n):
        if hard_map[i]:
            suspect.append(i)
        elif len(soft_map[i]) >= 2:
            suspect.append(i)

    # 归并成区间
    blocks = []
    for i in suspect:
        if blocks and i == blocks[-1][-1] + 1:
            blocks[-1].append(i)
        else:
            blocks.append([i])

    print("\n" + "=" * 66)
    print("检出 %d 个疑似广告段 (可疑切片 %d/%d)" % (len(blocks), len(suspect), n))
    print("=" * 66)
    for blk in blocks:
        a, b = blk[0], blk[-1]
        d0, d1 = segs[a]["start"], segs[b]["end"]
        print("▸ %s ~ %s  (段 #%d~#%d, %.2fs)"
              % (hms(d0), hms(d1), a, b, d1 - d0))
        rs = []
        for i in blk:
            rs += hard_map[i] + ["佐证:" + s for s in soft_map[i]]
        for r in dict.fromkeys(rs):
            print("    · %s" % r)
        # 把本次的广告 SPS 指纹打出来，方便往 KNOWN_AD_SPS 里补
        adfps = sorted({info[i].get("sps_md5") for i in blk
                        if info[i].get("sps_md5") and info[i]["sps_md5"] != host_md5})
        if adfps:
            print("    广告 SPS 指纹: %s" % ", ".join(adfps))
        if "--all" in sys.argv:
            for i in range(a, min(b + 1, n)):
                r = info[i]
                sp = r.get("sps")
                print("     #%-4d %s~%s dur=%.3f pts=%-10s %-14s %s"
                      % (i, hms(segs[i]["start"]), hms(segs[i]["end"]), segs[i]["dur"],
                         r.get("pts"),
                         ("%sx%s fps=%s" % (sp["w"], sp["h"], sp["fps"])) if sp else "无SPS",
                         (r.get("sps_md5") or "无")[:12]))

    return dict(segs=segs, info=info, cuts=cuts, blocks=blocks,
                host_md5=host_md5, main_pmt=main_pmt, main_enc=main_enc,
                main_fps=main_fps, main_cp=main_cp, mode=mode)


def hms(x):
    return "%d:%05.2f" % (x // 60, x % 60)


if __name__ == "__main__":
    if len(sys.argv) < 2:
        print(__doc__)
        sys.exit(1)
    url = sys.argv[1]

    # 解析 --workers / --probe（文档里承诺了这两个开关，不能是摆设）
    workers, probe = 16, 128 * 1024
    for flag, cast in (("--workers", int), ("--probe", int)):
        if flag in sys.argv:
            try:
                v = cast(sys.argv[sys.argv.index(flag) + 1])
            except (IndexError, ValueError):
                print("参数 %s 需要一个整数值" % flag)
                sys.exit(2)
            if flag == "--workers":
                workers = max(1, v)
            else:
                probe = max(64 * 1024, v)

    r = scan(url, workers=workers, probe=probe)
    if "--json" in sys.argv:
        p = sys.argv[sys.argv.index("--json") + 1]
        out = {
            "url": url,
            "segments": len(r["segs"]),
            "total_duration": r["segs"][-1]["end"] if r["segs"] else 0,
            "cuts": r["cuts"],
            "blocks": [
                {
                    "seg_from": b[0], "seg_to": b[-1],
                    "time_from": hms(r["segs"][b[0]]["start"]),
                    "time_to": hms(r["segs"][b[-1]]["end"]),
                    "seconds": round(r["segs"][b[-1]]["end"] - r["segs"][b[0]]["start"], 2),
                } for b in r["blocks"]
            ],
            "host_sps_md5": r["host_md5"],
            "main_encoder": r["main_enc"],
        }
        json.dump(out, open(p, "w"), ensure_ascii=False, indent=1)
        print("已写出", p)
