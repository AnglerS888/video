#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""内容级段特征（ffmpeg）—— 响度 / 静音 / 黑场 / 场景切换。

用于回答「这一段到底是不是插播内容」这种码流元数据答不了的问题。
"""
import sys
import os
import re
import subprocess
import tempfile
from concurrent.futures import ThreadPoolExecutor

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import adscan

FFMPEG = os.environ.get(
    "FFMPEG",
    r"C:/Users/Administrator/AppData/Local/Temp/workbuddy-update-x64-workbuddy/adscan_bin/ffmpeg.exe")


def analyze(url):
    data = adscan.http_get(url, timeout=60)
    fd, path = tempfile.mkstemp(suffix=".ts")
    os.close(fd)
    with open(path, "wb") as f:
        f.write(data)
    try:
        # ---- 音频：响度 + 静音 ----
        p = subprocess.run(
            [FFMPEG, "-hide_banner", "-i", path, "-vn",
             "-af", "volumedetect,silencedetect=noise=-50dB:d=0.3", "-f", "null", "-"],
            capture_output=True, text=True, encoding="utf-8", errors="replace", timeout=120)
        err = p.stderr
        mv = re.search(r"mean_volume:\s*(-?[\d.]+) dB", err)
        xv = re.search(r"max_volume:\s*(-?[\d.]+) dB", err)
        sil = re.findall(r"silence_duration:\s*([\d.]+)", err)

        # ---- 视频：黑场 + 场景切换 ----
        p2 = subprocess.run(
            [FFMPEG, "-hide_banner", "-i", path,
             "-vf", "blackdetect=d=0.3:pix_th=0.10,select='gt(scene,0.4)',showinfo",
             "-an", "-f", "null", "-"],
            capture_output=True, text=True, encoding="utf-8", errors="replace", timeout=120)
        e2 = p2.stderr
        blk = re.findall(r"black_duration:\s*([\d.]+)", e2)
        scenes = len(re.findall(r"n:\s*\d+ pts_time", e2))
        return {
            "size": len(data),
            "mean": float(mv.group(1)) if mv else None,
            "max": float(xv.group(1)) if xv else None,
            "sil": [float(x) for x in sil],
            "blk": [float(x) for x in blk],
            "scenes": scenes,
        }
    finally:
        os.remove(path)


def main():
    url = sys.argv[1]
    args = sys.argv[2:]
    if "--sample" in args:
        i = args.index("--sample")
        idxs = list(range(int(args[i + 1]), int(args[i + 2]) + 1))
        base = [300, 700, 1100, 1400]
    else:
        idxs = [int(x) for x in args]
        base = []
    media, lines = adscan.load_playlist(url)
    segs, total = adscan.parse_segments(lines, media)
    allidx = idxs + base

    res = {}
    with ThreadPoolExecutor(max_workers=4) as ex:
        futs = {ex.submit(analyze, segs[i]["url"]): i for i in allidx}
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
            print("#%-5d %-9s ERR %s" % (i, adscan.hms(segs[i]["start"]), r["err"]))
            return
        print("#%-5d %-9s size=%7d  vol(mean=%s max=%s)  静音%d段/%.2fs  黑场%d段  场景切换%d" % (
            i, adscan.hms(segs[i]["start"]), r["size"],
            r["mean"], r["max"], len(r["sil"]), sum(r["sil"]),
            len(r["blk"]), r["scenes"]))

    for i in idxs:
        show(i, "目标")
    if base:
        print("----- 正片基线 -----")
        for i in base:
            show(i, "基线")


if __name__ == "__main__":
    main()
