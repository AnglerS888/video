#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""时间点 → 画面 映射实验。

问题：播放器显示时间 T 时，屏幕上放的是哪一段的内容？
做法：ffmpeg -ss T 解 1 帧 → 8x8 灰度特征 → 与各段特征比对。

这能区分「playlist 累计时间轴」和「别的什么时间轴」。
"""
import sys
import os
import subprocess
import tempfile
from concurrent.futures import ThreadPoolExecutor

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import adscan
from phash import frames_of, dist, N, NFRAMES

FFMPEG = os.environ.get(
    "FFMPEG",
    r"C:/Users/ADMINI~1/AppData/Local/Temp/workbuddy-update-x64-workbuddy/adscan_bin/ffmpeg.exe")


def frame_at(url, t):
    """ffmpeg 按时间点 seek，解 1 帧，返回 64 维灰度向量"""
    p = subprocess.run(
        [FFMPEG, "-hide_banner", "-v", "error", "-ss", "%.3f" % t, "-i", url,
         "-frames:v", "1", "-vf", "scale=%d:%d,format=gray" % (N, N),
         "-f", "rawvideo", "-"],
        capture_output=True, timeout=180)
    raw = p.stdout
    if len(raw) < N * N:
        return None
    return list(raw[:N * N])


def main():
    m3u8 = sys.argv[1]
    media, lines = adscan.load_playlist(m3u8)
    segs, total = adscan.parse_segments(lines, media)

    # 取一段区间做参考特征
    lo, hi = int(sys.argv[2]), int(sys.argv[3])
    ts = [float(x) for x in sys.argv[4].split(",")]
    ref = {}
    with ThreadPoolExecutor(max_workers=6) as ex:
        futs = {ex.submit(frames_of, segs[i]["url"]): i for i in range(lo, hi + 1)}
        for f, i in futs.items():
            try:
                ref[i] = f.result()
            except Exception:
                ref[i] = []

    for t in ts:
        fr = frame_at(media, t)
        if fr is None:
            print("T=%.2f  解帧失败" % t)
            continue
        best = []
        for i in range(lo, hi + 1):
            fs = ref.get(i) or []
            if not fs:
                continue
            d = min(dist(fr, f) for f in fs)
            best.append((d, i))
        best.sort()
        print("T=%7.2f  →  最像: " % t +
              "  ".join("#%d(%.2f, %s) d=%.4f" % (i, segs[i]["start"],
                                                   adscan.hms(segs[i]["start"]), d)
                        for d, i in best[:3]))


if __name__ == "__main__":
    main()
