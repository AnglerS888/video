#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""探测 v13 在 13:16-13:28 对应切片，核查工具是否漏检。"""
import os, sys

for k in ("HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"):
    os.environ.pop(k, None)

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from adscan import load_playlist, parse_segments, probe_segment

url = sys.argv[1]
media, lines = load_playlist(url)
segs, total = parse_segments(lines, media)
for n, s in enumerate(segs):
    s["i"] = n
    s["t"] = s["start"]
    s["d"] = s["dur"]
    s["u"] = s["url"]
print("media:", media)
print("段数:", len(segs), "总长 %.2fs" % total)

def mmss(s):
    return "%d:%02d" % (int(s) // 60, int(s) % 60)

# 目标窗口：默认 13:16~13:28，可用命令行覆盖：probe_window.py <url> <起始秒> <结束秒>
if len(sys.argv) >= 4:
    win = (float(sys.argv[2]), float(sys.argv[3]))
else:
    win = (13 * 60 + 16, 13 * 60 + 28)
target = [s for s in segs if s["t"] < win[1] and s["t"] + s["d"] > win[0]]
print("窗口 %s~%s 覆盖段: %s" % (mmss(win[0]), mmss(win[1]),
                                  [s["i"] for s in target]))
if not target:
    target = segs[398:405]

idx_target = [s["i"] for s in target]
lo, hi = min(idx_target), max(idx_target)
idx = list(range(lo - 3, hi + 4)) + [200, 600, 1000, 1400]

print()
print("%-6s %-8s %-10s %-34s %-8s %-6s %s" % ("段", "起点", "时长", "SPS指纹", "fps", "宽", "编码器"))
rows = []
for i in idx:
    if i < 0 or i >= len(segs):
        continue
    s = segs[i]
    r = probe_segment(s["u"], 128 * 1024)
    tag = ""
    if i in idx_target:
        tag = "  <<< 窗口内"
    elif i in (200, 600, 1000, 1400):
        tag = "  (正片基线)"
    print("%-6d %-8s %-10.3f %-34s %-8s %-6s %s%s" % (
        i, mmss(s["t"]), s["d"],
        (r.get("sps_md5") or "?")[:32], r.get("fps"), r.get("width"),
        (r.get("encoder") or "")[:26], tag))
    rows.append((i, r))

print()
sps = {}
for i, r in rows:
    sps.setdefault(r.get("sps_md5"), []).append(i)
print("SPS 指纹分组:")
for k, v in sorted(sps.items(), key=lambda x: -len(x[1])):
    print("   %s  -> 段 %s" % ((k or "?")[:32], v[:12]))

pmt = {}
for i, r in rows:
    pmt.setdefault(r.get("pmt_fp"), []).append(i)
print("PMT 指纹分组:")
for k, v in sorted(pmt.items(), key=lambda x: -len(x[1])):
    print("   %s  -> 段 %s" % ((k or "?")[:32], v[:12]))
