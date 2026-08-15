#!/usr/bin/env python3
"""Generate the per-frame SVGs for the animated wormhole logomark.

The faceted gateway ring (a true-3D, ridge-cross-section take on the mark in
generate-logomark.py) sits at a 3/4 angle in a wide social-card frame. One loop:

    closed crystalline spiral iris opens -> wormhole ignites -> a water/plasma
    "burst" erupts along the gate normal, expands and retracts -> the event
    horizon settles and fades -> iris closes -> back to the start (seamless).

This script only emits the frame SVGs. The raster/video pipeline (resvg +
ImageMagick + ffmpeg -> GIF / WebP / MP4) lives in render-wormhole-animation.sh;
see README.md. Geometry, palette, timing, and the iris are all tunable constants
at the top of this file.

    python3 generate-wormhole-animation.py [--transparent] [OUTDIR]

--transparent omits the background (for the alpha WebP). OUTDIR defaults to a
frames/ (or frames_t/) directory next to this script.
"""
import math
import os
import random
import sys

# --transparent omits the background rect (used for the alpha WebP build).
TRANSPARENT = "--transparent" in sys.argv

# ---- ring geometry (ported from docs/assets/generate-logomark.py) ----------
M = 10
# Gate sits left of frame-centre, positioned so the full gate+plume content is
# horizontally centred (equal left/right padding) when the burst is at peak.
CX, CY = 74.0, 48.0
GATE_SCALE = 0.82                 # gate size (shrunk to fit the frame with margin)
RO_TIP, RO_BASE = 47.0 * GATE_SCALE, 36.0 * GATE_SCALE
RI_BASE, RI_TIP = 29.0 * GATE_SCALE, 17.0 * GATE_SCALE
BLUE, TEAL = (50, 108, 229), (45, 212, 191)
LIGHT = (-0.5, -0.86)
SEAM = "#0B1220"
FLOOR = 0.46

# ---- true-3D view + animation parameters -----------------------------------
YAW = math.radians(32)            # rotate the upright gate about its vertical axis
PITCH = math.radians(10.5)        # tilt so the gate faces down-right; thick side up-left
THICK = 8.0 * GATE_SCALE          # gate thickness (logo units), extruded along normal
KXR = math.cos(YAW)               # horizontal foreshorten factor for the wormhole
N_FRAMES = 64
BG = "#0B1220"
# social-card canvas: 1.91:1 (Open Graph / Twitter large image, 1200x630).
VIEW_W, VIEW_H = 190.5, 100.0

_cY, _sY = math.cos(YAW), math.sin(YAW)
_cP, _sP = math.cos(PITCH), math.sin(PITCH)
# Ridge cross-section: the outer rim AND the inner hole edge live at z=0 and the
# band peaks to +/-RIDGE_H in the middle. The gate centre (CX,CY,0) maps to the
# canvas centre under rotation, so no recentre shift is needed.
SHIFTX, SHIFTY = 0.0, 0.0
RIDGE_H = 4.6                      # peak half-depth at the middle of the ring band

# 3D light direction (screen frame: x right, y up, z toward viewer): top-left-front
_L3 = (-0.42, 0.55, 0.72)
_l3n = math.sqrt(sum(c * c for c in _L3))
L3 = tuple(c / _l3n for c in _L3)


def project3d(px, py, z=0.0):
    """Project a gate-plane point (logo coords) at depth z (along the face
    normal) through yaw-then-pitch rotation to screen (sx, sy) + view depth.

    Local axes: X right, Y up (logo y is down, so Y = -(py-50)), Z toward viewer.
    """
    X, Y, Z = px - CX, -(py - CY), z
    x1 = X * _cY + Z * _sY                    # rotate about vertical (Y) axis
    z1 = -X * _sY + Z * _cY
    y2 = Y * _cP - z1 * _sP                   # rotate about horizontal (X) axis
    z2 = Y * _sP + z1 * _cP
    return CX + x1 + SHIFTX, CY - y2 + SHIFTY, z2


def normal_z_edge(g0, g1):
    """View-depth (z) of a rim segment's TRUE outward normal after rotation.
    Positive => that wall turns toward the viewer (draw it); else it is hidden.
    Uses the real edge normal, not a radial proxy, so slanted spike edges cull
    correctly (a radial proxy leaves X-ray seams on the spikes)."""
    dx, dy = g1[0] - g0[0], g1[1] - g0[1]
    nx, ny = dy, -dx                          # perpendicular to the edge
    mx, my = (g0[0] + g1[0]) / 2, (g0[1] + g1[1]) / 2
    if (mx - CX) * nx + (my - CY) * ny < 0:   # orient outward
        nx, ny = -nx, -ny
    return -ny * _sP - nx * _sY * _cP         # rotated z of (nx,-ny,0)


# Wormhole/burst centre = the gate centre on the z=0 plane (the inner edge).
MCX, MCY, _ = project3d(CX, CY, 0.0)

# blast direction: the gate's face normal projected to screen (down-right).
_bx, _by = _sY, _cY * _sP
_bm = math.hypot(_bx, _by) or 1.0
BDX, BDY = _bx / _bm, _by / _bm
# unit perpendicular to the blast (for billow rise + droplet spread)
PDX, PDY = -BDY, BDX

# Affine map from gate-plane (logo) coords to screen at the FRONT aperture
# (z=+T/2). Drawing the wormhole/glow as plain circles inside this transform
# makes them share the ring's exact projected tilt AND sit in the visible hole,
# instead of an axis-aligned ellipse parked behind the ring body.
_ma, _mb = _cY, -_sY * _sP
_mc, _md = 0.0, _cP
_me = CX * (1 - _ma)
_mf = CY - _mb * CX - _md * CY
GATE_MATRIX = "matrix(%.5f %.5f %.5f %.5f %.5f %.5f)" % (_ma, _mb, _mc, _md, _me, _mf)


def _lerp(a, b, t):
    return tuple(a[i] + (b[i] - a[i]) * t for i in range(3))


def _clamp(v):
    return max(0, min(255, int(round(v))))


def _hex(c):
    return "#%02x%02x%02x" % (_clamp(c[0]), _clamp(c[1]), _clamp(c[2]))


def _rot(x, y, ang):
    """Rotate (x,y) in the gate plane about its center by ang radians (dialing)."""
    dx, dy = x - CX, y - CY
    c, s = math.cos(ang), math.sin(ang)
    return CX + dx * c - dy * s, CY + dx * s + dy * c


# ---- facet geometry ---------------------------------------------------------
def _facets(ang):
    n = 2 * M
    O, I = [], []
    for k in range(n):
        a = -math.pi / 2 + k * math.pi / M
        ro = RO_TIP if k % 2 == 0 else RO_BASE
        ri = RI_TIP if k % 2 == 1 else RI_BASE
        O.append(_rot(CX + ro * math.cos(a), CY + ro * math.sin(a), ang))
        I.append(_rot(CX + ri * math.cos(a), CY + ri * math.sin(a), ang))
    tris = []
    for k in range(n):
        o0, o1, i0, i1 = O[k], O[(k + 1) % n], I[k], I[(k + 1) % n]
        tris.append((o0, o1, i0, "o", k))
        tris.append((o1, i1, i0, "i", k))
    return tris


def _shade(t, glow):
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
    col = [c * f for c in base]
    if kind == "i" and glow > 0:                    # wormhole light on inner facets
        col = _lerp(col, (190, 245, 255), 0.5 * glow)
    return _hex(tuple(col))


def _rim_local(ang):
    """Outer star vertices in gate-plane (logo) coords, rotated by the dial."""
    n = 2 * M
    pts = []
    for k in range(n):
        a = -math.pi / 2 + k * math.pi / M
        ro = RO_TIP if k % 2 == 0 else RO_BASE
        pts.append(_rot(CX + ro * math.cos(a), CY + ro * math.sin(a), ang))
    return pts


def _rim_inner(ang):
    """Inner-star vertices (the hole edge) in gate-plane coords, rotated."""
    n = 2 * M
    pts = []
    for k in range(n):
        a = -math.pi / 2 + k * math.pi / M
        ri = RI_TIP if k % 2 == 1 else RI_BASE
        pts.append(_rot(CX + ri * math.cos(a), CY + ri * math.sin(a), ang))
    return pts


def hole_path(ang):
    """Inner-edge (z=0) star polygon -- the wormhole opening we see through."""
    pts = [project3d(gx, gy, 0.0)[:2] for gx, gy in _rim_inner(ang)]
    return "M" + " L".join("%.2f %.2f" % p for p in pts) + " Z"


def rotate3d(gx, gy, z):
    """Gate-plane point + depth z -> rotated 3D coords (x right, y up, z to viewer)."""
    X, Y, Z = gx - CX, -(gy - CY), z
    x1 = X * _cY + Z * _sY
    z1 = -X * _sY + Z * _cY
    y2 = Y * _cP - z1 * _sP
    z2 = Y * _sP + z1 * _cP
    return x1, y2, z2


def _tri(p0, p1, p2, glow, inner):
    """Build one ridge facet from gate-plane+z verts: returns (depth, path, fill).

    Lit by the real 3D facet normal so the ridge catches light like cut crystal.
    """
    a, b, c = rotate3d(*p0), rotate3d(*p1), rotate3d(*p2)
    ux, uy, uz = b[0] - a[0], b[1] - a[1], b[2] - a[2]
    wx, wy, wz = c[0] - a[0], c[1] - a[1], c[2] - a[2]
    nx, ny, nz = uy * wz - uz * wy, uz * wx - ux * wz, ux * wy - uy * wx
    nl = math.sqrt(nx * nx + ny * ny + nz * nz) or 1.0
    nx, ny, nz = nx / nl, ny / nl, nz / nl
    if nz < 0:                                        # orient toward the viewer
        nx, ny, nz = -nx, -ny, -nz
    d = nx * L3[0] + ny * L3[1] + nz * L3[2]
    yc = (p0[1] + p1[1] + p2[1]) / 3.0
    base = _lerp(BLUE, TEAL, (yc - 3) / 94.0)
    f = max(FLOOR, 0.42 + 0.95 * max(0.0, d))
    col = [ch * f for ch in base]
    if inner and glow > 0:                            # wormhole light on inner facets
        col = _lerp(col, (190, 245, 255), 0.5 * glow)
    s0 = (CX + a[0] + SHIFTX, CY - a[1] + SHIFTY)
    s1 = (CX + b[0] + SHIFTX, CY - b[1] + SHIFTY)
    s2 = (CX + c[0] + SHIFTX, CY - c[1] + SHIFTY)
    path = "M%.2f %.2f L%.2f %.2f L%.2f %.2f Z" % (
        s0[0], s0[1], s1[0], s1[1], s2[0], s2[1])
    depth = (a[2] + b[2] + c[2]) / 3.0
    return depth, path, _hex(tuple(col))


def ring_svg(ang, glow):
    """Faceted gem ring: knife-edge (z=0) at the outer rim AND the inner hole,
    rising to a ridge (+/-RIDGE_H) down the middle of the band."""
    ro = _rim_local(ang)
    ri = _rim_inner(ang)
    n = 2 * M
    mid = [((ro[k][0] + ri[k][0]) / 2.0, (ro[k][1] + ri[k][1]) / 2.0)
           for k in range(n)]
    H = RIDGE_H
    faces = []
    for k in range(n):
        j = (k + 1) % n
        Ok = (ro[k][0], ro[k][1], 0.0); Oj = (ro[j][0], ro[j][1], 0.0)
        Ik = (ri[k][0], ri[k][1], 0.0); Ij = (ri[j][0], ri[j][1], 0.0)
        for hz in (H, -H):                            # front ridge, then back ridge
            Mk = (mid[k][0], mid[k][1], hz); Mj = (mid[j][0], mid[j][1], hz)
            # outer band: rim edge -> ridge
            faces.append(_tri(Ok, Oj, Mj, glow, False))
            faces.append(_tri(Ok, Mj, Mk, glow, False))
            # inner band: ridge -> hole edge
            faces.append(_tri(Mk, Mj, Ij, glow, True))
            faces.append(_tri(Mk, Ij, Ik, glow, True))
    faces.sort(key=lambda f: f[0])                    # painter's: far (small z) first
    return "\n".join(
        '<path d="%s" fill="%s" stroke="%s" stroke-width="0.3" '
        'stroke-linejoin="round"/>' % (path, col, SEAM)
        for _depth, path, col in faces)


# ---- crystalline spiral iris diaphragm -------------------------------------
# Overlapping spiral iris blades, restyled as our faceted blue-steel
# shards (not smooth grey trinium): they wind to a spiky pinwheel when shut.
IRIS_BLADES = 20                  # = the inner star's 20 vertices; a fine spiral
IRIS_ROUT = 30.0                  # rim radius (covers the star), gate-plane
IRIS_WIND = math.radians(92)      # angular sweep of each blade, rim -> tip
IRIS_TIPMIN = 0.9                 # tip radius when shut (small spiky centre)
IRIS_STEEL = (104, 124, 156)      # base blue-steel
_IRIS_LA = math.atan2(LIGHT[1], LIGHT[0])   # light direction (top-left)


def iris_svg(ang, o):
    """Stylised crystalline spiral iris filling the gate aperture (o=0 shut,
    o=1 open): overlapping blue-steel shards winding to a spiky pinwheel centre.
    Drawn in the gate plane (clipped to the hole) so it tilts and dials."""
    if o >= 0.985:
        return ""
    a = IRIS_TIPMIN + o * (IRIS_ROUT - IRIS_TIPMIN)   # tip radius (centre -> rim)
    step = 2 * math.pi / IRIS_BLADES
    span = step * 2.3                                  # rim width -> blades overlap
    out = ['<g clip-path="url(#hole)"><g transform="%s">' % GATE_MATRIX]
    for k in range(IRIS_BLADES):
        b = -math.pi / 2 + k * step + ang
        r1 = (CX + IRIS_ROUT * math.cos(b), CY + IRIS_ROUT * math.sin(b))
        r2 = (CX + IRIS_ROUT * math.cos(b + span),
              CY + IRIS_ROUT * math.sin(b + span))
        t = (CX + a * math.cos(b + IRIS_WIND), CY + a * math.sin(b + IRIS_WIND))
        # bezier controls bow both edges into the spiral
        c1 = (CX + IRIS_ROUT * 0.50 * math.cos(b + IRIS_WIND * 0.42),
              CY + IRIS_ROUT * 0.50 * math.sin(b + IRIS_WIND * 0.42))
        c2 = (CX + IRIS_ROUT * 0.62 * math.cos(b + span * 0.75 + IRIS_WIND * 0.1),
              CY + IRIS_ROUT * 0.62 * math.sin(b + span * 0.75 + IRIS_WIND * 0.1))
        d = ("M%.2f %.2f Q%.2f %.2f %.2f %.2f Q%.2f %.2f %.2f %.2f L%.2f %.2f Z"
             % (r1[0], r1[1], c1[0], c1[1], t[0], t[1],
                c2[0], c2[1], r2[0], r2[1], r1[0], r1[1]))
        # crystalline blue-steel, lit from the top-left like the ring facets
        f = 0.5 + 0.6 * max(0.0, math.cos(b - _IRIS_LA))
        out.append('<path d="%s" fill="%s" stroke="#161d29" stroke-width="0.35" '
                   'stroke-linejoin="round"/>'
                   % (d, _hex(tuple(c * f for c in IRIS_STEEL))))
        # bright lit edge along the leading (spiral) curve -> crystalline sheen
        out.append('<path d="M%.2f %.2f Q%.2f %.2f %.2f %.2f" fill="none" '
                   'stroke="#d4e3f4" stroke-width="0.45" opacity="0.55"/>'
                   % (r1[0], r1[1], c1[0], c1[1], t[0], t[1]))
    out.append('</g></g>')
    return "\n".join(out)


# ---- animation envelopes ----------------------------------------------------
def smoothstep(a, b, x):
    if x <= a:
        return 0.0
    if x >= b:
        return 1.0
    t = (x - a) / (b - a)
    return t * t * (3 - 2 * t)


def lerp1(a, b, t):
    return a + (b - a) * t


def envelopes(t):
    # iris diaphragm: 0=closed, 1=open. Closed at the loop boundary; opens just
    # before the wormhole ignites; closes after the plume has retracted + faded.
    iris = smoothstep(0.05, 0.15, t) * (1 - smoothstep(0.74, 0.85, t))
    ig = smoothstep(0.14, 0.21, t) * (1 - smoothstep(0.22, 0.30, t))
    # burst: a violent punch-out (fast rise) that overshoots, then collapses
    rise = smoothstep(0.16, 0.25, t)
    fall = smoothstep(0.30, 0.50, t)
    burst = (rise * (1 - fall)) ** 0.7
    eh = smoothstep(0.17, 0.32, t) * (1 - smoothstep(0.64, 0.76, t))
    charge = smoothstep(0.10, 0.17, t) * (1 - smoothstep(0.17, 0.24, t))
    glow = max(ig, eh * 0.6, charge * 0.4, burst * 0.9)
    return dict(iris=iris, ig=ig, burst=burst, eh=eh, charge=charge, glow=glow)


# ---- the horizontal burst (billowing cloud blasting out the front) --------
# radius profile along the blast axis: (u, radius). Narrow at the gate mouth,
# billowing to a bulbous head near the far end, rounding off at the tip.
# narrow neck at the gate -> billowing bulbous mushroom head (like the film)
# Near-cylindrical: emerges from the whole event horizon at ~full width, stays
# roughly constant, then swells a little toward a rounded end (not a cone).
# Base (u=0) radius = the event-horizon white ring (~18, foreshortened to ~15x18
# at the gate); it widens slightly as the plume billows out, then rounds at end.
CLOUD_PROFILE = [(0.0, 18.0), (0.25, 19.5), (0.50, 21.0), (0.72, 22.5),
                 (0.88, 24.0), (1.0, 20.0)]
# No centerline lift: the plume travels straight along the gate normal and the
# round blobs expand symmetrically about that axis (top rises == bottom drops).
BURST_RISE = 0.0


def _profile(u):
    pts = CLOUD_PROFILE
    for i in range(len(pts) - 1):
        u0, r0 = pts[i]
        u1, r1 = pts[i + 1]
        if u <= u1:
            return lerp1(r0, r1, (u - u0) / (u1 - u0))
    return pts[-1][1]


def cloud_blobs(burst):
    """Overlapping blobs along the blast axis -> a billowing cloud silhouette."""
    L = 52 * burst                       # blast reach (violent punch-out)
    n = 20
    blobs = []
    for i in range(n):
        u = i / (n - 1)
        d = L * u
        # emerge along the gate normal, then rise (-PD) along the whole length so
        # the plume stays level/buoyant instead of drooping at the middle/end
        # emerge level out of the ring (descent ramps in), so the base sits
        # symmetrically on the white ring before curving down-right as it extends
        ramp = smoothstep(0.0, 0.32, u)
        x = MCX + d * BDX
        y = MCY + d * BDY * ramp
        r = _profile(u) * (0.45 + 0.55 * burst)
        # base blob is foreshortened (KXR) to match the event-horizon ellipse it
        # emerges from, easing to round as the plume pulls toward the viewer
        fx = KXR + (1.0 - KXR) * smoothstep(0.0, 0.4, u)
        blobs.append((x, y, r, fx))
    return blobs, L


# deterministic spray droplets: (position along length u, jitter, size)
random.seed(7)
DROPS = [(random.uniform(0.12, 1.0), random.uniform(-1.0, 1.0),
          random.uniform(0.8, 2.3)) for _ in range(26)]


def droplets(burst):
    """Spray flung off the top/leading edge of the erupting vortex."""
    out = []
    L = 52 * burst
    for (u, jit, sz) in DROPS:
        d = L * u + jit * 4
        rad = _profile(u) * (0.45 + 0.55 * burst)     # cloud radius here
        off = rad * (0.82 + 0.28 * jit)             # sit on / just past top edge
        lift = BURST_RISE * burst * u                    # follow the plume's rise
        x = MCX + d * BDX - (off + lift) * PDX
        y = MCY + d * BDY - (off + lift) * PDY
        r = sz * (0.4 + 0.6 * burst)
        op = max(0.0, burst - 0.3)
        out.append('<ellipse cx="%.2f" cy="%.2f" rx="%.2f" ry="%.2f" '
                   'fill="url(#cloud)" opacity="%.2f"/>' % (x, y, r, r, op))
    return "\n".join(out)


def defs(seed, eh, hole):
    return f'''  <defs>
    <clipPath id="hole"><path d="{hole}"/></clipPath>
    <radialGradient id="eh" cx="50%" cy="50%" r="60%">
      <stop offset="0%" stop-color="#fbffff"/>
      <stop offset="22%" stop-color="#c4f7f4"/>
      <stop offset="48%" stop-color="#5cd3ec"/>
      <stop offset="74%" stop-color="#2f9be0"/>
      <stop offset="100%" stop-color="#163f6a"/>
    </radialGradient>
    <linearGradient id="cloud" x1="0" y1="0" x2="1" y2="0">
      <stop offset="0%" stop-color="#cdf4fb"/>
      <stop offset="50%" stop-color="#79d7ea"/>
      <stop offset="100%" stop-color="#3fa9da"/>
    </linearGradient>
    <radialGradient id="glow" cx="50%" cy="50%" r="50%">
      <stop offset="0%" stop-color="#bff3ff" stop-opacity="0.95"/>
      <stop offset="55%" stop-color="#3fd0e0" stop-opacity="0.40"/>
      <stop offset="100%" stop-color="#3fd0e0" stop-opacity="0"/>
    </radialGradient>
    <radialGradient id="iris" cx="42%" cy="38%" r="62%">
      <stop offset="0%" stop-color="#5a6678"/>
      <stop offset="42%" stop-color="#8b97ab"/>
      <stop offset="74%" stop-color="#454f63"/>
      <stop offset="100%" stop-color="#262d3b"/>
    </radialGradient>
    <!-- watery shimmer: translucent blue-white patches that ADD texture over
         the clean gradient without translating its bright centre -->
    <filter id="shimmer" x="-20%" y="-20%" width="140%" height="140%">
      <feTurbulence type="fractalNoise" baseFrequency="0.10 0.14"
        numOctaves="3" seed="{seed}" result="n"/>
      <feColorMatrix in="n" type="matrix"
        values="0 0 0 0 0.80  0 0 0 0 0.92  0 0 0 0 1  0.6 0 0 0 -0.2"/>
    </filter>
    <!-- plasma/water surface: gently warp the body, then layer flowing caustic
         highlights (bright cyan-white) and deep-blue troughs from the same
         noise so the surface varies in colour + value without splashing -->
    <filter id="burst" x="-45%" y="-90%" width="190%" height="290%">
      <feTurbulence type="turbulence" baseFrequency="0.05 0.08"
        numOctaves="4" seed="{seed+13}" result="n"/>
      <feDisplacementMap in="SourceGraphic" in2="n" scale="3.5"
        xChannelSelector="R" yChannelSelector="G" result="warp"/>
      <feColorMatrix in="n" type="matrix" values="0 0 0 0 0.86  0 0 0 0 0.99
        0 0 0 0 1  2.2 0 0 0 -0.78" result="hi"/>
      <feComposite in="hi" in2="warp" operator="in" result="hiIn"/>
      <feColorMatrix in="n" type="matrix" values="0 0 0 0 0.04  0 0 0 0 0.20
        0 0 0 0 0.50  -2.0 0 0 0 0.62" result="lo"/>
      <feComposite in="lo" in2="warp" operator="in" result="loIn"/>
      <feMerge>
        <feMergeNode in="warp"/><feMergeNode in="loIn"/><feMergeNode in="hiIn"/>
      </feMerge>
    </filter>
    <!-- finer caustic ripple veins, clipped to the body -->
    <filter id="ripples" x="-45%" y="-90%" width="190%" height="290%">
      <feTurbulence type="turbulence" baseFrequency="0.13 0.17"
        numOctaves="3" seed="{seed+41}" result="n"/>
      <feColorMatrix in="n" type="matrix" values="0 0 0 0 0.88  0 0 0 0 0.98
        0 0 0 0 1  2.4 0 0 0 -1.15" result="hi"/>
      <feComposite in="hi" in2="SourceGraphic" operator="in"/>
    </filter>
    <filter id="churn" x="-45%" y="-90%" width="190%" height="290%">
      <feTurbulence type="fractalNoise" baseFrequency="0.12 0.16"
        numOctaves="3" seed="{seed+31}" result="n"/>
      <feDisplacementMap in="SourceGraphic" in2="n" scale="5"
        xChannelSelector="R" yChannelSelector="G"/>
    </filter>
    <filter id="soft" x="-80%" y="-80%" width="260%" height="260%">
      <feGaussianBlur stdDeviation="3.2"/>
    </filter>
  </defs>'''


def frame_svg(i):
    t = i / N_FRAMES
    e = envelopes(t)
    ang = math.radians(36.0 * t)          # one symmetry period -> seamless dial
    seed = 1 + i
    hole = hole_path(ang)

    p = [f'<svg xmlns="http://www.w3.org/2000/svg" '
         f'viewBox="0 0 {VIEW_W:g} {VIEW_H:g}">']
    p.append(defs(seed, e["eh"], hole))
    if not TRANSPARENT:
        p.append(f'<rect width="{VIEW_W:g}" height="{VIEW_H:g}" fill="{BG}"/>')

    # back glow behind the gate (drawn in the gate plane -> tilts with the ring)
    gr = (24 + 8 * e["glow"]) * GATE_SCALE
    p.append('<g transform="%s" opacity="%.2f"><circle cx="%.1f" cy="%.1f" r="%.1f" '
             'fill="url(#glow)" filter="url(#soft)"/></g>'
             % (GATE_MATRIX, 0.16 + 0.5 * e["glow"], CX, CY, gr))

    # the upright 3/4 gateway ring (hero)
    p.append('<g>%s</g>' % ring_svg(ang, e["glow"]))

    # event-horizon surface filling the aperture (clipped to the star hole).
    # Its bright center is recessed to the gate's back plane (WH_DX/DY) so it
    # sits at the ring's optical centre, not the front-face geometric centre.
    if e["eh"] > 0.02:
        op = min(1.0, e["eh"] * 1.15)
        # outer clip is the real front aperture (screen space); inside, the
        # surface is a plain circle in the gate plane -> correct tilted ellipse.
        p.append('<g clip-path="url(#hole)" opacity="%.2f">' % op)
        # dark base: any displacement tear falls back to gate-shadow, not bg
        p.append('<path d="%s" fill="#0e2a47"/>' % hole)
        p.append('<g transform="%s">' % GATE_MATRIX)
        # clean radial gradient -> bright white core stays DEAD-CENTRE on (CX,CY)
        p.append('<circle cx="%.1f" cy="%.1f" r="35" fill="url(#eh)"/>' % (CX, CY))
        # watery shimmer texture on top (adds churn without moving the centre)
        p.append('<circle cx="%.1f" cy="%.1f" r="35" fill="#dffaff" '
                 'filter="url(#shimmer)" opacity="%.2f"/>' % (CX, CY, 0.5 * e["eh"]))
        # concentric ripples radiating from the centre (the shimmering pool look)
        for j in range(4):
            ph = (t * 3.0 + j / 4.0) % 1.0     # 3 cycles/loop -> seamless
            rr = 3.0 + ph * 27.0
            rop = (1 - ph) * 0.33 * e["eh"]
            p.append('<circle cx="%.1f" cy="%.1f" r="%.2f" fill="none" '
                     'stroke="#e8feff" stroke-width="%.2f" opacity="%.3f"/>'
                     % (CX, CY, rr, 0.4 + 1.1 * (1 - ph), rop))
        # specular highlight, dead-centred on the gate
        p.append('<circle cx="%.1f" cy="%.1f" r="%.1f" fill="url(#glow)" '
                 'opacity="%.2f"/>' % (CX, CY, 12 * e["eh"], 0.5 * e["eh"]))
        p.append('</g></g>')

    # crystalline spiral iris over the aperture (covers the wormhole when shut)
    p.append(iris_svg(ang, e["iris"]))

    # ignition shock ring (a gate-plane circle -> tilts with the ring)
    if e["ig"] > 0.02:
        sr = (14 + 26 * (1 - e["ig"])) * GATE_SCALE
        p.append('<g transform="%s"><circle cx="%.1f" cy="%.1f" r="%.1f" fill="none" '
                 'stroke="#cdfbff" stroke-width="%.2f" opacity="%.2f"/></g>'
                 % (GATE_MATRIX, CX, CY, sr, 1.8 * e["ig"], 0.6 * e["ig"]))

    # --- the burst: billowing cloud blasting horizontally out the front ----
    if e["burst"] > 0.01:
        blobs, L = cloud_blobs(e["burst"])
        circ = "".join('<ellipse cx="%.2f" cy="%.2f" rx="%.2f" ry="%.2f"/>'
                       % (x, y, r * fx, r) for (x, y, r, fx) in blobs)
        # bright highlight blobs along the core (inner, smaller)
        hi = "".join('<ellipse cx="%.2f" cy="%.2f" rx="%.2f" ry="%.2f"/>'
                     % (x, y, r * 0.55 * fx, r * 0.55) for (x, y, r, fx) in blobs)
        kop = min(1.0, e["burst"] * 1.1)
        # A spacetime distortion: one contiguous mass with a flowing water/plasma
        # surface (colour + caustic variation), no splashing or droplets.
        # soft volume haze (cohesive body)
        p.append('<g fill="url(#cloud)" opacity="%.2f" filter="url(#soft)">%s</g>'
                 % (0.32 * kop, circ))
        # plasma body: warped + caustic highlights + deep-blue troughs
        p.append('<g fill="url(#cloud)" opacity="%.2f" filter="url(#burst)">%s</g>'
                 % (0.9 * kop, circ))
        # finer caustic ripples flowing over the surface
        p.append('<g fill="url(#cloud)" opacity="%.2f" filter="url(#ripples)">%s</g>'
                 % (0.55 * kop, circ))
        # soft bright core near the gate mouth (calm, not chaotic)
        p.append('<g fill="#eafdff" opacity="%.2f" filter="url(#soft)">%s</g>'
                 % (0.18 * kop, hi))
        # bright mouth where it draws out of the event horizon
        p.append('<ellipse cx="%.2f" cy="%.2f" rx="%.1f" ry="%.1f" fill="url(#glow)" '
                 'opacity="%.2f"/>'
                 % (MCX, MCY, 15.3 * e["burst"], 18.0 * e["burst"], 0.8 * e["burst"]))

    p.append('</svg>')
    return "\n".join(p)


def main():
    # positional arg (if any) is the output dir; default sits next to this file
    pos = [a for a in sys.argv[1:] if not a.startswith("-")]
    if pos:
        outdir = pos[0]
    else:
        sub = "frames_t" if TRANSPARENT else "frames"
        outdir = os.path.join(os.path.dirname(__file__), sub)
    os.makedirs(outdir, exist_ok=True)
    for i in range(N_FRAMES):
        with open(os.path.join(outdir, "f%03d.svg" % i), "w") as fh:
            fh.write(frame_svg(i))
    print("wrote %d frame SVGs to %s" % (N_FRAMES, outdir))


if __name__ == "__main__":
    main()
