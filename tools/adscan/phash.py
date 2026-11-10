#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""画面感知比对 —— 纯 Python，无 numpy。

每段抽 4 帧缩到 8x8 灰度当特征向量，比较两段的画面相似度。
用途：判断「这两段是不是同一段素材」（广告复用）。
"""
import sys
import os
import math
import subprocess
import tempfile
from concurrent.futures import ThreadPoolExecutor

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import adscan

FFMPEG = os.environ.get(
    "FFMPEG",
    r"C:/Users/ADMINI~1/AppData/Local/Temp/workbuddy-update-x64-workbuddy/adscan_bin/ffmpeg.exe")

N = 8          # 特征图边长
NFRAMES = 4    # 每段抽帧数


def frames_of(url):
    """抽 NFRAMES 帧 → 每帧 64 维灰度向量"""
    data = adscan.http_get(url, timeout=60)
    fd, path = tempfile.mkstemp(suffix=".ts")
    os.close(fd)
    with open(path, "wb") as f:
        f.write(data)
    try:
        p = subprocess.run(
            [FFMPEG, "-hide_banner", "-v", "error", "-i", path,
             "-vf", "fps=%d,scale=%d:%d,format=gray" % (NFRAMES, N, N),
             "-frames:v", str(NFRAMES), "-f", "rawvideo", "-"],
            capture_output=True, timeout=120)
        raw = p.stdout
        out = []
        for k in range(NFRAMES):
            chunk = raw[k * N * N:(k + 1) * N * N]
            if len(chunk) == N * N:
                out.append(list(chunk))
        return out
    finally:
        os.remove(path)


def dist(a, b):
    """归一化欧氏距离（对亮度整体偏移不敏感）"""
    ma = sum(a) / len(a)
    mb = sum(b) / len(b)
    sa = math.sqrt(sum((x - ma) ** 2 for x in a)) or 1
    sb = math.sqrt(sum((x - mb) ** 2 for x in b)) or 1
    return math.sqrt(sum((x / sa - y / sb) ** 2 for x, y in zip(a, b))) / len(a)


def seg_dist(fa, fb):
    """两组帧序列的平均距离（取最优时序对齐）"""
    if not fa or not fb:
        return 99.0
    best = 0.0
    for i in range(len(fa)):
        for j in range(len(fb)):
            best = max(best, 0)  # noop
    tot = 0
    cnt = 0
    for i in range(len(fa)):
        j = min(len(fb) - 1, int(i * len(fb) / len(fa)))
        tot += dist(fa[i], fb[j])
        cnt += 1
    return tot / cnt


def load(url, idxs):
    media, lines = adscan.load_playlist(url)
    segs, total = adscan.parse_segments(lines, media)
    res = {}
    with ThreadPoolExecutor(max_workers=6) as ex:
        futs = {ex.submit(frames_of, segs[i]["url"]): i for i in idxs}
        for f, i in futs.items():
            try:
                res[i] = f.result()
            except Exception as e:
                res[i] = []
    return segs, res


def main():
    srcC = "https://v13.wsyzym3u8.com/202610/25/MEt06UAxyG27/video/index.m3u8"
    srcA = "https://v15.wsyzym3u8.com/202609/30/D0B1D0qnTG27/video/index.m3u8"

    c_idx = list(range(383, 408)) + [300, 700, 1100, 1400]
    a_idx = list(range(154, 162))

    segsC, resC = load(srcC, c_idx)
    segsA, resA = load(srcA, a_idx)
    print("源A 广告段画面特征（已知广告）:")
    for i in a_idx:
        print("   A#%-4d %-9s 帧数=%d" % (i, adscan.hms(segsA[i]["start"]), len(resA.get(i) or [])))
    print()
    print("源C 各段 与「源A广告」的画面距离（越小越像同一素材）:")
    rows = []
    for i in c_idx:
        f = resC.get(i) or []
        if not f:
            print("   C#%-4d %-9s 无帧" % (i, adscan.hms(segsC[i]["start"])))
            continue
        d = min(seg_dist(f, resA[j]) for j in a_idx if resA.get(j))
        rows.append((d, i))
    for d, i in sorted(rows):
        print("   C#%-4d %-9s 距离=%.4f %s" % (i, adscan.hms(segsC[i]["start"]), d,
                                              "  <<< 与源A广告高度相似" if d < 0.55 else ""))


if __name__ == "__main__":
    main()
