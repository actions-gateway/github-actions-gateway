#!/usr/bin/env python3
"""Generate the GitHub Actions Gateway logomark SVG masters.

The mark is a faceted "gateway ring" — a crystalline torus in the brand
Kubernetes-blue -> teal gradient, lit from the top-left for depth. Everything is
parametric (point count, spike depth, contrast, light angle) so the mark can be
retuned without hand-editing SVG.

Run from docs/assets/:

    python3 generate-logomark.py

then re-render the rasters with resvg (see README.md). Outputs:
  logo.svg       header logomark (transparent)
  icon-tile.svg  opaque navy tile for the iOS / PWA rasters
  favicon.svg    simplified smooth ring (legible at 16 px)
"""
import math

M = 10                       # number of spikes around the ring
CX = CY = 50.0
RO_TIP, RO_BASE = 47.0, 36.0  # outer star (spikes out / valleys)
RI_BASE, RI_TIP = 29.0, 17.0  # inner star (spikes into the hole)
BLUE, TEAL = (50, 108, 229), (45, 212, 191)
LIGHT = (-0.5, -0.86)         # light direction (top-left)
SEAM = "#0B1220"
FLOOR = 0.46                  # min brightness so shadow facets read on a dark header


def _lerp(a, b, t):
    return tuple(a[i] + (b[i] - a[i]) * t for i in range(3))


def _clamp(v):
    return max(0, min(255, int(round(v))))


def _hex(c):
    return "#%02x%02x%02x" % (_clamp(c[0]), _clamp(c[1]), _clamp(c[2]))


def _facets():
    n = 2 * M
    O, I = [], []
    for k in range(n):
        a = -math.pi / 2 + k * math.pi / M
        ro = RO_TIP if k % 2 == 0 else RO_BASE
        ri = RI_TIP if k % 2 == 1 else RI_BASE
        O.append((CX + ro * math.cos(a), CY + ro * math.sin(a)))
        I.append((CX + ri * math.cos(a), CY + ri * math.sin(a)))
    tris = []
    for k in range(n):
        o0, o1, i0, i1 = O[k], O[(k + 1) % n], I[k], I[(k + 1) % n]
        tris.append((o0, o1, i0, "o", k))
        tris.append((o1, i1, i0, "i", k))
    return tris


def _shade(t):
    p, kind, k = t[:3], t[3], t[4]
    xc = sum(q[0] for q in p) / 3.0
    yc = sum(q[1] for q in p) / 3.0
    base = _lerp(BLUE, TEAL, (yc - 3) / 94.0)
    dx, dy = xc - CX, yc - CY
    dl = math.hypot(dx, dy) or 1.0
    light = (dx / dl) * LIGHT[0] + (dy / dl) * LIGHT[1]
    facet = 1.28 if kind == "o" else 0.6
    facet *= 1.1 if k % 2 == 0 else 0.92
    f = max(FLOOR, facet * (0.5 + 0.62 * max(0.0, light)))
    return _hex(tuple(c * f for c in base))


def ring_paths(stroke_width=0.35, indent="  "):
    out = []
    for t in _facets():
        p = t[:3]
        d = "M%.2f %.2f L%.2f %.2f L%.2f %.2f Z" % (
            p[0][0], p[0][1], p[1][0], p[1][1], p[2][0], p[2][1])
        out.append('%s<path d="%s" fill="%s" stroke="%s" stroke-width="%s" '
                    'stroke-linejoin="round"/>' % (indent, d, _shade(t), SEAM, stroke_width))
    return "\n".join(out)


def write_logo():
    svg = ('<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100" '
           'role="img" aria-label="GitHub Actions Gateway logo">\n%s\n</svg>\n'
           % ring_paths())
    open("logo.svg", "w").write(svg)


def write_icon_tile():
    svg = ('<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100" '
           'role="img" aria-label="GitHub Actions Gateway">\n'
           '  <rect width="100" height="100" fill="#0B1220"/>\n'
           '  <g transform="translate(18,18) scale(0.64)">\n%s\n  </g>\n</svg>\n'
           % ring_paths(stroke_width=0.5, indent="    "))
    open("icon-tile.svg", "w").write(svg)


def write_favicon():
    # Simplified: a smooth blue->teal donut on a navy tile, legible at 16 px.
    svg = (
        '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" '
        'role="img" aria-label="GitHub Actions Gateway">\n'
        '  <defs>\n'
        '    <linearGradient id="g" x1="16" y1="4" x2="16" y2="28" gradientUnits="userSpaceOnUse">\n'
        '      <stop offset="0" stop-color="#3b8bff"/>\n'
        '      <stop offset="1" stop-color="#2DD4BF"/>\n'
        '    </linearGradient>\n'
        '  </defs>\n'
        '  <rect width="32" height="32" rx="7" fill="#0B1220"/>\n'
        '  <circle cx="16" cy="16" r="10.5" fill="url(#g)"/>\n'
        '  <circle cx="16" cy="16" r="5" fill="#0B1220"/>\n'
        '</svg>\n')
    open("favicon.svg", "w").write(svg)


def social_ring_group():
    """The ring as a placed <g>, inline with the social card's kicker."""
    return ('  <g transform="translate(80,123) scale(0.34)">\n%s\n  </g>'
            % ring_paths(stroke_width=0.9, indent="    "))


if __name__ == "__main__":
    write_logo()
    write_icon_tile()
    write_favicon()
    print("wrote logo.svg, icon-tile.svg, favicon.svg")
