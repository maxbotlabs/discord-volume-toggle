#!/usr/bin/env python3
"""Generate 4 volume-level tray icons (muted, low, mid, high) as speaker glyphs."""
from PIL import Image, ImageDraw

SIZE = 64  # render large, downscale for crispness

def speaker_icon(level):
    """level: 0=muted, 1=low, 2=mid, 3=high (number of sound bars)."""
    img = Image.new("RGBA", (SIZE, SIZE), (0, 0, 0, 0))
    d = ImageDraw.Draw(img)

    # Colors
    cream = (245, 240, 232, 255)
    gold = (200, 168, 74, 255)
    muted_red = (200, 80, 80, 255)

    # Speaker body (trapezoid)
    d.polygon([(12, 26), (12, 38), (22, 38), (22, 26)], fill=cream)
    # Speaker cone (triangle)
    d.polygon([(22, 26), (22, 38), (32, 44), (32, 20)], fill=cream)

    if level == 0:
        # Muted: draw an X over the speaker
        d.line([(36, 22), (52, 42)], fill=muted_red, width=5)
        d.line([(52, 22), (36, 42)], fill=muted_red, width=5)
    else:
        # Sound bars (arcs) — number depends on level
        cx, cy = 32, 32
        for i in range(level):
            r = 12 + i * 8
            bbox = [cx + 4 - r, cy - r, cx + 4 + r, cy + r]
            d.arc(bbox, start=-45, end=45, fill=gold, width=5)

    return img

if __name__ == "__main__":
    for level, name in [(0, "muted"), (1, "low"), (2, "mid"), (3, "high")]:
        img = speaker_icon(level)
        # Save 16 and 32 px versions
        img.resize((16, 16), Image.LANCZOS).save(f"tray-{name}-16.png")
        img.resize((32, 32), Image.LANCZOS).save(f"tray-{name}-32.png")
        img.resize((64, 64), Image.LANCZOS).save(f"tray-{name}-64.png")
    print("tray icons generated")
