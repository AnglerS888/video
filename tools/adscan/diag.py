#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""逐段全维度特征 dump —— 诊断用（找出工具漏检/误检的真实原因）。

用法:
    python diag.py <m3u8> <段号起始> <段号结束> [更多段号...]
    python diag.py <m3u8> --range 380 415
    python diag.py <m3u8> --sample 380 415   # 另附 5 个正片基线段

比 adscan 多出：段体积、PCR、帧类型分布、音频 ADTS 参数、视频码率。
"""
import sys
import os
import hashlib
from collections import Counter
from concurrent.futures import ThreadPoolExecutor

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import adscan

ADTS_RATES = [96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050,
               16000, 12000, 11025, 8000, 7350]


def scan_raw(data):
    """底层扫描：拿 PCR、帧类型、音频 ADTS。"""
    vpid = apid = None
    pes = {}          # pid -> list of payload chunks
    pts = None
    pcrs = []
    n = len(data) // 188
    for k in range(n):
        p = data[k * 188:(k + 1) * 188]
        if len(p) < 188 or p[0] != 0x47:
            continue
        pid = ((p[1] & 0x1F) << 8) | p[2]
        pusi = bool(p[1] & 0x40)
        afc = (p[3] >> 4) & 0x3
        off = 4
        if afc & 2:
            if p[4] >= 183:
                continue
            if p[4] > 0 and (p[5] & 0x10):        # PCR flag
                b = p[6:12]
                pcr = ((b[0] << 25) | (b[1] << 17) | (b[2] << 9) |
                       (b[3] << 1) | (b[4] >> 7))
                pcrs.append(pcr)
            off += 1 + p[4]
        if off >= 188 or not (afc & 1):
            continue
        pl = p[off:]
        if not pl:
            continue
        if pusi and len(pl) >= 9 and pl[:3] == b"\x00\x00\x01":
            sid = pl[3]
            if sid in (0xE0, 0xE1, 0x20, 0x21, 0x40, 0xFD) and vpid is None:
                vpid = pid
            elif sid in (0xC0, 0xC1, 0xBD) and apid is None:
                apid = pid
        if vpid is not None and pid == vpid:
            if pusi and len(pl) >= 9 and pl[:3] == b"\x00\x00\x01":
                if pl[7] & 0x80 and len(pl) >= 14 and pts is None:
                    t = 0
                    for b in pl[9:14]:
                        t = (t << 8) | b
                    pts = ((t >> 33) & 0x7) << 30 | ((t >> 1) & 0x7FFFFFFF)
                pes.setdefault(pid, []).append(pl[9 + pl[8]:])
            else:
                pes.setdefault(pid, []).append(pl)
        elif apid is not None and pid == apid:
            pes.setdefault(pid, []).append(pl[9 + pl[8]:] if (
                pusi and len(pl) >= 9 and pl[:3] == b"\x00\x00\x01") else pl)

    # ---- 视频 NAL 类型统计 ----
    nal_types = Counter()
    for chunk in pes.get(vpid, []):
        for nn in adscan.split_annexb(chunk):
            if nn:
                nal_types[nn[0] & 0x1F] += 1

    # ---- 音频 ADTS ----
    a_rate = a_ch = a_frames = 0
    for chunk in pes.get(apid, []):
        b = bytes(chunk)
        i = 0
        while i + 7 <= len(b):
            if b[i] == 0xFF and (b[i + 1] & 0xF0) == 0xF0:
                ri = (b[i + 2] >> 2) & 0x0F
                ch = ((b[i + 2] & 0x01) << 2) | ((b[i + 3] >> 6) & 0x03)
                flen = ((b[i + 3] & 0x03) << 11) | (b[i + 4] << 3) | (b[i + 5] >> 5)
                if ri < len(ADTS_RATES) and flen > 7:
                    if not a_rate:
                        a_rate, a_ch = ADTS_RATES[ri], ch
                    a_frames += 1
                    i += flen
                    continue
            i += 1
    return dict(pts=pts, nal=nal_types, pcr=pcrs,
                a_rate=a_rate, a_ch=a_ch, a_frames=a_frames,
                vpid=vpid, apid=apid)


def probe(url):
    data = adscan.http_get(url, timeout=60)
    r = adscan.parse_ts(data)
    raw = scan_raw(data)
    sp = r.get("sps") or {}
    return {
        "size": len(data),
        "sps_md5": r.get("sps_md5"),
        "w": sp.get("w"), "h": sp.get("h"), "fps": sp.get("fps"),
        "cp": sp.get("cp"), "mc": sp.get("mc"),
        "pmt_fp": r.get("pmt_fp"),
        "pts": raw["pts"],
        "sei": (r.get("sei") or [None])[0],
        "nal": raw["nal"],
        "pcr0": raw["pcr"][0] if raw["pcr"] else None,
        "pcrn": raw["pcr"][-1] if raw["pcr"] else None,
        "a_rate": raw["a_rate"], "a_ch": raw["a_ch"], "a_frames": raw["a_frames"],
    }


def main():
    url = sys.argv[1]
    args = sys.argv[2:]
    if "--range" in args:
        i = args.index("--range")
        idxs = list(range(int(args[i + 1]), int(args[i + 2]) + 1))
        base = []
    elif "--sample" in args:
        i = args.index("--sample")
        idxs = list(range(int(args[i + 1]), int(args[i + 2]) + 1))
        base = [300, 700, 1100, 1400, 200]
    else:
        idxs = [int(x) for x in args]
        base = []

    media, lines = adscan.load_playlist(url)
    segs, total = adscan.parse_segments(lines, media)
    print("媒体: %s" % media)
    print("段数 %d  总时长 %.2fs" % (len(segs), total))
    print()

    allidx = idxs + base
    res = {}
    with ThreadPoolExecutor(max_workers=8) as ex:
        futs = {ex.submit(probe, segs[i]["url"]): i for i in allidx}
        for f in futs:
            pass
        for f, i in futs.items():
            try:
                res[i] = f.result()
            except Exception as e:
                res[i] = {"err": str(e)}

    def show(i, tag):
        r = res.get(i) or {}
        if "err" in r:
            print("#%-5d %s ERR %s" % (i, adscan.hms(segs[i]["start"]), r["err"]))
            return
        nal = r["nal"]
        vid_n = sum(nal.values())
        i_ = nal.get(5, 0)
        print("#%-5d %s dur=%.3f size=%8d sps=%s %sx%s fps=%s cp=%s pmt=%s" % (
            i, adscan.hms(segs[i]["start"]), segs[i]["dur"], r["size"],
            (r["sps_md5"] or "None")[:10], r["w"], r["h"], r["fps"], r["cp"],
            (r["pmt_fp"] or "None")[:10]))
        print("        pts=%s pcr0=%s pcrn=%s  nal(I=%d/非I=%d/总=%d)  a=%sHz %sch f=%d  enc=%s" % (
            r["pts"], r["pcr0"], r["pcrn"], i_, vid_n - i_, vid_n,
            r["a_rate"], r["a_ch"], r["a_frames"], (r["sei"] or "-")[:28]))

    for i in idxs:
        show(i, "目标")
    if base:
        print("\n----- 正片基线段 -----")
        for i in base:
            show(i, "基线")


if __name__ == "__main__":
    main()
