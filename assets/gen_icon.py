#!/usr/bin/env python3
"""Generate a clean volume/speaker icon for Discord Volume Toggle."""
from PIL import Image, ImageDraw

SIZE = 256

def make_icon():
    img = Image.new("RGBA", (SIZE, SIZE), (0, 0, 0, 0))
    d = ImageDraw.Draw(img)

    # Rounded-square background with a vertical gradient (deep teal -> darker).
    # Draw a rounded rect by drawing a filled rounded rectangle.
    radius = 48
    # Gradient background
    top = (26, 58, 58)      # deep teal
    bottom = (16, 36, 40)   # darker
    for y in range(SIZE):
        t = y / SIZE
        r = int(top[0] + (bottom[0] - top[0]) * t)
        g = int(top[1] + (bottom[1] - top[1]) * t)
        b = int(top[2] + (bottom[2] - top[2]) * t)
        d.line([(0, y), (SIZE, y)], fill=(r, g, b, 255))

    # Rounded corners: punch out transparent corners
    mask = Image.new("L", (SIZE, SIZE), 0)
    md = ImageDraw.Draw(mask)
    md.rounded_rectangle([0, 0, SIZE - 1, SIZE - 1], radius=radius, fill=255)
    img.putalpha(mask)

    # Re-draw gradient onto the masked image (so corners are rounded)
    # Actually simpler: draw gradient, then apply rounded mask.
    # We'll rebuild cleanly.
    img = Image.new("RGBA", (SIZE, SIZE), (0, 0, 0, 0))
    d = ImageDraw.Draw(img)
    for y in range(SIZE):
        t = y / SIZE
        r = int(top[0] + (bottom[0] - top[0]) * t)
        g = int(top[1] + (bottom[1] - top[1]) * t)
        b = int(top[2] + (bottom[2] - top[2]) * t)
        d.line([(0, y), (SIZE, y)], fill=(r, g, b, 255))
    mask = Image.new("L", (SIZE, SIZE), 0)
    md = ImageDraw.Draw(mask)
    md.rounded_rectangle([0, 0, SIZE - 1, SIZE - 1], radius=radius, fill=255)
    img.putalpha(mask)

    d = ImageDraw.Draw(img)

    # Gold accent color
    gold = (200, 168, 74, 255)
    cream = (245, 240, 232, 255)

    # Speaker body (trapezoid) pointing right
    # Speaker cone: polygon from left
    speaker = [(48, 108), (48, 148), (88, 148), (88, 108)]
    d.polygon(speaker, fill=cream)

    # Speaker cone (triangle) to the right
    cone = [(88, 108), (88, 148), (128, 168), (128, 88)]
    d.polygon(cone, fill=cream)

    # Sound waves (arcs) in gold
    # Three arcs to the right of the cone
    cx, cy = 128, 128
    for i, r in enumerate([44, 62, 80]):
        bbox = [cx + 8 - r, cy - r, cx + 8 + r, cy + r]
        # draw arc from -50 to 50 degrees
        d.arc(bbox, start=-50, end=50, fill=gold, width=10)

    return img

if __name__ == "__main__":
    img = make_icon()
    img.save("icon-256.png")
    # Also save smaller sizes for the ico
    for s in (16, 24, 32, 48, 64, 128):
        img.resize((s, s), Image.LANCZOS).save(f"icon-{s}.png")
    print("icons generated")
