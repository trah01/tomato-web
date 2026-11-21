#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# pyright: reportMissingImports=false
"""
build_font_map.py — 生成番茄字体混淆还原映射表

原理
----
番茄把正文里一部分汉字替换成私用区（PUA，U+E000–U+F8FF）码位，再下发一个
把 PUA 码位映射到「错误字形」的 woff2 字体。浏览器按字形渲染，人眼看到正确
汉字，但文本层是乱码。

还原方法：把混淆字体每个 glyph 的轮廓指令序列做指纹，与一份标准
（未混淆的）思源黑体做比对，指纹相同即「同一字形」，从而得到
PUA 码位 → 真实汉字 的映射。

用法
----
  python3 build_font_map.py --obf fq.woff2 --ref SourceHanSansSC-Regular.otf \
      --out data/font_map.json

  # 若没有标准字体，可从下面任一来源取得：
  #   https://github.com/adobe-fonts/source-han-sans/releases
  #   macOS: /System/Library/Fonts/Supplemental/Songti.ttc（骨架不同，不推荐）

  # 自动化：直接从番茄页面抓字体
  python3 build_font_map.py --fetch --ref ./ref.otf --out data/font_map.json
"""
from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import sys
import urllib.request

try:
    from fontTools.ttLib import TTFont
    from fontTools.pens.recordingPen import DecomposingRecordingPen
except ImportError:
    sys.exit("需要 fonttools： pip install fonttools brotli")


def glyph_signature(font: TTFont, glyph_name: str) -> str | None:
    """用轮廓指令序列 + 归一化坐标生成字形指纹。"""
    try:
        gs = font.getGlyphSet()
        pen = DecomposingRecordingPen(gs)
        gs[glyph_name].draw(pen)
    except Exception:
        return None

    parts: list[str] = []
    for op, args in pen.value:
        parts.append(op)
        for pt in args:
            if pt is None:
                continue
            try:
                parts.append(f"{round(float(pt[0]), 1)},{round(float(pt[1]), 1)}")
            except (TypeError, IndexError):
                parts.append(str(pt))
    digest = hashlib.md5("|".join(parts).encode()).hexdigest()
    return digest


def build_ref_index(ref_font: TTFont) -> dict[str, str]:
    """标准字体：字形指纹 → 汉字。"""
    idx: dict[str, str] = {}
    cmap = ref_font.getBestCmap()
    for cp, gname in cmap.items():
        if 0x4E00 <= cp <= 0x9FFF or 0x3400 <= cp <= 0x4DBF:
            sig = glyph_signature(ref_font, gname)
            if sig:
                idx.setdefault(sig, chr(cp))
    return idx


def open_font(path: str, index: int = 0) -> TTFont:
    """打开字体；TTC 字体集合需要指定索引。"""
    try:
        return TTFont(path, fontNumber=index)
    except Exception as exc:  # noqa: BLE001
        if "collection" in str(exc).lower() or "font number" in str(exc).lower():
            # 集合字体：返回首个可用的中文字体
            for i in range(16):
                try:
                    f = TTFont(path, fontNumber=i)
                    cmap = f.getBestCmap()
                    if any(0x4E00 <= cp <= 0x9FFF for cp in cmap):
                        return f
                except Exception:  # noqa: BLE001, PERF203
                    continue
        raise


def build_map(obf_path: str, ref_path: str) -> dict[str, str]:
    obf = open_font(obf_path)
    ref = open_font(ref_path)

    ref_idx = build_ref_index(ref)
    if not ref_idx:
        raise SystemExit("标准字体索引为空，确认 --ref 是完整的中文字体")

    out: dict[str, str] = {}
    missing = 0
    for cp, gname in obf.getBestCmap().items():
        if not (0xE000 <= cp <= 0xF8FF):
            continue
        sig = glyph_signature(obf, gname)
        real = ref_idx.get(sig) if sig else None
        if real:
            out[chr(cp)] = real
        else:
            missing += 1
    print(f"匹配成功 {len(out)} 个，未匹配 {missing} 个（{len(out)+missing} 总计）")
    return out


def fetch_font_from_page(reader_url: str, dst: str) -> str:
    """从番茄章节页抓取当前混淆字体 woff2。"""
    req = urllib.request.Request(
        reader_url,
        headers={
            "User-Agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) "
            "AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
            "Referer": "https://fanqienovel.com/",
        },
    )
    html = urllib.request.urlopen(req, timeout=30).read().decode("utf-8", "ignore")

    m = re.search(r'awesome-font/c/([0-9a-f]+)\.woff2', html)
    if not m:
        raise SystemExit("页面里没找到字体地址，确认 URL 是有效章节页")
    font_url = f"https://lf6-awef.bytetos.com/obj/awesome-font/c/{m.group(1)}.woff2"
    print(f"字体地址: {font_url}")

    urllib.request.urlretrieve(font_url, dst)
    print(f"已保存: {dst} ({os.path.getsize(dst)} bytes)")
    return dst


def main() -> None:
    ap = argparse.ArgumentParser(description="生成番茄字体映射表")
    ap.add_argument("--obf", help="混淆字体路径（woff2/woff/ttf）")
    ap.add_argument("--ref", required=True, help="标准中文字体路径（otf/ttf）")
    ap.add_argument("--out", default="data/font_map.json", help="输出 JSON")
    ap.add_argument("--fetch", action="store_true", help="自动从番茄页面抓字体")
    ap.add_argument("--page", default="", help="--fetch 使用的章节页 URL")
    args = ap.parse_args()

    obf = args.obf
    if args.fetch:
        page = args.page or "https://fanqienovel.com/reader/7179597681915134464"
        obf = fetch_font_from_page(page, "/tmp/fq_obf.woff2")
    if not obf:
        sys.exit("需要 --obf 或 --fetch")

    mapping = build_map(obf, args.ref)

    try:
        os.makedirs(os.path.dirname(args.out) or ".", exist_ok=True)
        with open(args.out, "w", encoding="utf-8") as fh:
            json.dump(mapping, fh, ensure_ascii=False, indent=0, sort_keys=True)
    except OSError as exc:
        sys.exit(f"无法写出 {args.out}: {exc}")
    print(f"已写入: {args.out}")

    sample = list(mapping.items())[:10]
    for k, v in sample:
        print(f"  {k!r} -> {v}")


if __name__ == "__main__":
    main()
