#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# pyright: reportMissingImports=false
"""
build_font_map_img.py — 用字形渲染比对生成番茄字体映射表（Robust 版）

为什么不用轮廓坐标比对
----------------------
番茄下发的混淆字体对字形做过轮廓重排/子集化，坐标与标准字体对不上，
直接比 pen 指令序列会 0 命中。可靠做法是「渲染成位图再比对」：

  1. 用混淆字体把每个 PUA 码位渲染成 NxN 灰度图
  2. 用标准思源黑体把候选汉字（常用字集）同样渲染
  3. 对两张图做归一化后计算像素差异（MSE / IoU），取最小者

为了控制计算量，先用字形宽高比和黑色像素占比做粗筛，再对候选做精比。

依赖
----
  pip install fonttools pillow numpy brotli
  # 需一份标准思源黑体：
  #   https://github.com/adobe-fonts/source-han-sans/releases

用法
----
  python3 build_font_map_img.py \
      --obf /tmp/fq_obf.woff2 \
      --ref /tmp/shs/OTF/SimplifiedChinese/SourceHanSansSC-Regular.otf \
      --out data/font_map.json
"""
from __future__ import annotations

import argparse
import json
import os
import sys

try:
    import numpy as np
    from PIL import Image, ImageDraw, ImageFont
    from fontTools.ttLib import TTFont
except ImportError:
    sys.exit("需要依赖： pip install fonttools pillow numpy brotli")

SIZE = 64          # 渲染基准尺寸
THRESHOLD = 0.06   # 粗筛阈值（宽高比/密度差）
TOPK = 6           # 精比候选数


def render_glyph_by_codepoint(font_path, codepoint: int, size: int = SIZE):
    """把单个字符渲染成归一化灰度矩阵。"""
    try:
        font = ImageFont.truetype(font_path, size)
    except OSError:
        return None
    img = Image.new("L", (size * 2, size * 2), 0)
    d = ImageDraw.Draw(img)
    try:
        d.text((size // 2, size // 2), chr(codepoint), font=font, fill=255)
    except Exception:
        return None
    arr = np.asarray(img, dtype=np.float32) / 255.0
    if arr.max() < 1e-6:
        return None
    # 裁到墨迹包围盒再缩放到统一尺寸，消除位置/字号差异
    ys, xs = np.nonzero(arr > 0.15)
    if len(ys) == 0:
        return None
    box = arr[ys.min():ys.max() + 1, xs.min():xs.max() + 1]
    if box.size == 0:
        return None
    img2 = Image.fromarray((box * 255).astype(np.uint8)).resize((size, size), Image.LANCZOS)
    out = np.asarray(img2, dtype=np.float32) / 255.0
    return out


def features(arr):
    """粗筛特征：墨迹密度的行列投影。"""
    return np.concatenate([
        arr.sum(axis=0) / arr.shape[0],
        arr.sum(axis=1) / arr.shape[1],
    ])


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--obf", required=True, help="混淆字体")
    ap.add_argument("--ref", required=True, help="标准思源黑体")
    ap.add_argument("--out", default="data/font_map.json")
    ap.add_argument("--charset", default="", help="候选汉字文件（每行一个），默认 3500 常用字")
    ap.add_argument("--limit", type=int, default=0, help="仅处理前 N 个 PUA（调试）")
    args = ap.parse_args()

    obf = TTFont(args.obf)
    pua = [cp for cp in obf.getBestCmap() if 0xE000 <= cp <= 0xF8FF]
    if args.limit:
        pua = pua[: args.limit]
    print(f"待还原 PUA 码位: {len(pua)}")

    # 候选汉字集
    if args.charset:
        try:
            with open(args.charset, encoding="utf-8") as fh:
                candidates = [c for line in fh for c in line.strip()]
        except OSError as exc:
            sys.exit(f"无法读取字表 {args.charset}: {exc}")
    else:
        candidates = [chr(cp) for cp in range(0x4E00, 0x9FA6)]
    candidates = list(dict.fromkeys(candidates))
    print(f"候选汉字: {len(candidates)}")

    # 预算候选的特征与位图
    print("渲染候选字…")
    ref_cache = {}
    ref_feat = []
    kept = []
    for ch in candidates:
        arr = render_glyph_by_codepoint(args.ref, ord(ch))
        if arr is None:
            continue
        ref_cache[ch] = arr
        ref_feat.append(features(arr))
        kept.append(ch)
    ref_feat = np.stack(ref_feat) if ref_feat else np.zeros((0, SIZE * 2))
    print(f"有效候选: {len(kept)}")

    mapping = {}
    unmatched = []
    for i, cp in enumerate(pua):
        arr = render_glyph_by_codepoint(args.obf, cp)
        if arr is None:
            unmatched.append(cp)
            continue
        f = features(arr)
        dist = np.abs(ref_feat - f).mean(axis=1)
        idxs = np.argsort(dist)[:TOPK]

        best_ch, best_score = None, 1e9
        for j in idxs:
            if dist[j] > THRESHOLD * 8:
                continue
            cand = ref_cache[kept[j]]
            try:
                score = float(((arr - cand) ** 2).mean())
            except (TypeError, ValueError):
                continue
            if score < best_score:
                best_score, best_ch = score, kept[j]
        if best_ch is not None and best_score < 0.05:
            mapping[chr(cp)] = best_ch
        else:
            unmatched.append(cp)

        if (i + 1) % 50 == 0:
            print(f"  进度 {i+1}/{len(pua)}  已匹配 {len(mapping)}")

    print(f"匹配成功 {len(mapping)}，未匹配 {len(unmatched)}")

    try:
        os.makedirs(os.path.dirname(args.out) or ".", exist_ok=True)
        with open(args.out, "w", encoding="utf-8") as fh:
            json.dump(mapping, fh, ensure_ascii=False, indent=0, sort_keys=True)
    except OSError as exc:
        sys.exit(f"无法写出 {args.out}: {exc}")
    print(f"已写入 {args.out}")
    for k, v in list(mapping.items())[:12]:
        print(f"  {k!r} -> {v}")


if __name__ == "__main__":
    main()
