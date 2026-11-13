"""Paints docs/logo.png, the logo of Atoll: an atoll seen from the air, as a photograph would show it.

    python docs/logo.py [output.png] [size in pixels]

It needs numpy and Pillow, and nothing of Atoll: it is run by hand, when the logo is to change.

Nothing here is drawn. Everything is worked out from depths and heights, as the sea and the light would: a reef crest
whose line is never regular, its slope into the deep, a reef flat, four islets with their beaches and their trees,
the disks of one pool, a lagoon that deepens toward its middle, coral heads, two passes, and the surf where the swell
meets the reef. The same seed gives the same picture at any size.
"""
import math
import sys

import numpy as np
from PIL import Image

OUT = sys.argv[1] if len(sys.argv) > 1 else 'docs/logo.png'
SIZE = int(sys.argv[2]) if len(sys.argv) > 2 else 512
S = max(1024, 4 * SIZE)     # painted four times as large, then brought down: no line is jagged
U = 256.0                   # the picture is 256 units wide
PX = S / U                  # pixels in a unit
rng = np.random.default_rng(20261113)

yy, xx = np.mgrid[0:S, 0:S].astype(np.float32)
X = (xx + 0.5) / PX
Y = (yy + 0.5) / PX


def value_noise(cells):
    """Smooth noise in about [0, 1], with that many features across the picture."""
    c = max(1, int(round(cells)))
    grid = rng.random((c + 3, c + 3)).astype(np.float32)
    big = int(math.ceil(S * (c + 3) / c))
    a = np.asarray(Image.fromarray(grid).resize((big, big), Image.BICUBIC), dtype=np.float32)
    o = int(S / c)
    return a[o:o + S, o:o + S]


def fbm(cells, octaves=4, gain=0.5):
    """Noise of several scales, in about [-0.5, 0.5]."""
    total = np.zeros((S, S), np.float32)
    amp, norm = 1.0, 0.0
    for o in range(octaves):
        total += amp * (value_noise(cells * 2 ** o) - 0.5)
        norm += amp
        amp *= gain
    return total / norm


def smoothstep(a, b, x):
    t = np.clip((x - a) / (b - a), 0.0, 1.0)
    return t * t * (3 - 2 * t)


def mix(a, b, t):
    return a + (b - a) * t


def blur(a, units):
    """A Gaussian blur of a field, by a radius in units."""
    sigma = units * PX
    fy = np.fft.fftfreq(S)[:, None]
    fx = np.fft.rfftfreq(S)[None, :]
    kernel = np.exp(-2 * (math.pi * sigma) ** 2 * (fx * fx + fy * fy))
    return np.fft.irfft2(np.fft.rfft2(a) * kernel, s=(S, S)).astype(np.float32)


def ring_noise(theta, harmonics, beta=1.0):
    """Noise around the ring, in about [-1, 1]: the sum of a few waves of the angle."""
    out = np.zeros_like(theta)
    power = 0.0
    for k in harmonics:
        a = k ** -beta
        out += a * np.cos(k * theta + rng.uniform(0, 2 * math.pi))
        power += a * a / 2
    return out / (2.2 * math.sqrt(power))


# The sea bends every line a little, at two scales: no curve of the picture is the one its formula gives.
warp_x = fbm(3, 3) * 20 + fbm(11, 3) * 8
warp_y = fbm(3, 3) * 20 + fbm(11, 3) * 8
XW, YW = X + warp_x, Y + warp_y

CX, CY = 130.0, 127.0
R0 = 88.0
rho = np.hypot(XW - CX, YW - CY)
theta = np.arctan2(YW - CY, XW - CX)    # 0 is east, a quarter turn is south: y grows downward
deg = np.degrees(theta) % 360.0


def crest(angle):
    """The reef's crest: longer from the south-west to the north-east, and never quite regular."""
    r = np.ones_like(angle)
    for k, a, phase in [(1, 0.030, 200), (2, 0.105, 318), (3, 0.060, 80), (4, 0.030, 20), (5, 0.020, 300), (6, 0.012, 140)]:
        r += a * np.cos(k * (angle - math.radians(phase)))
    return R0 * r


r_crest = crest(theta)
d = rho - r_crest                       # across the reef: positive toward the ocean, negative toward the lagoon

# The reef flat, wider here and narrower there.
flat = 9.5 + 3.4 * ring_noise(theta, range(2, 8))

# Four islets, the disks of one pool. For each: where it starts and how far round it goes, in degrees clockwise from
# east; how wide it gets; how far it sits from the crest; toward which end it is widest; how blunt its ends are.
ISLETS = [(236, 62, 27.0, 0.4, 0.80, 0.50), (318, 58, 20.0, 1.6, 1.40, 0.60), (44, 70, 19.0, 0.6, 0.70, 0.50),
          (148, 40, 24.0, 1.0, 1.00, 0.40)]
# The passes, where the reef opens: their middle, half their width, and how they slant across the rim.
PASSES = [(30, 6.5, 0.22), (212, 10.0, -0.30)]

# land is how far inside an islet a point is, and how far from one when negative.
land = np.full((S, S), -99.0, np.float32)
lagoon_side = np.zeros((S, S), np.float32)
to_tip = np.full((S, S), 99.0, np.float32)
for start, span, widest, off, skew, blunt in ISLETS:
    t = ((deg - start) % 360.0) / span
    t = np.where(t > 1 + (360 - span) / (2 * span), t - 360.0 / span, t)    # the nearest way round
    tt = np.clip(t, 0, 1)
    taper = np.clip(np.sin(math.pi * tt ** skew), 0, 1) ** blunt
    p1, p2, p3 = rng.uniform(0, 1, 3)
    uneven = 0.5 + 0.5 * (0.6 * np.cos(2 * math.pi * (1.3 * tt + p1)) + 0.4 * np.cos(2 * math.pi * (2.9 * tt + p2)))
    width = widest * taper * (0.72 + 0.5 * uneven)
    middle = -(1.6 + off) - 0.5 * width - 1.2 * np.cos(2 * math.pi * (0.8 * tt + p3))
    inside = 0.5 * width - np.abs(d - middle)
    tip = np.minimum(t, 1 - t) * span * (math.pi / 180) * r_crest
    inside = np.minimum(inside, tip * 0.8 + 0.3)
    lagoon_side = np.where(inside > land, smoothstep(-2.0, 2.0, middle - d), lagoon_side)
    to_tip = np.where(inside > land, tip, to_tip)
    land = np.maximum(land, inside)
land += 4.4 * fbm(17, 3) + 1.3 * fbm(100, 2)

# How far the lagoon's edge runs shallow before it drops: sand aprons, in lobes, wider behind the islets.
near_land = blur(np.clip(land + 4, 0, 6) / 6, 6.0)
apron = 2.5 + 9.0 * np.clip(0.5 + 1.7 * fbm(7, 3), 0, 1) ** 1.5 + 7.0 * near_land
s = -d - (flat + apron)                 # into the lagoon, past the shallows

# Depths, in metres: the reef flat, the slope outside with its grooves, the lagoon by two steps and its veins of reef.
z_flat = 0.4 + 2.6 * np.clip(0.5 + 1.3 * fbm(34, 4), 0, 1) ** 1.4
z_out = 0.35 + 0.9 * np.clip(d, 0, None) ** 1.55
groove = ring_noise(theta, rng.choice(np.arange(90, 230), 26, replace=False), 0.2)
z_out *= 1 + 0.28 * groove * np.exp(-((d - 3.2) ** 2) / 12)
reach = 34.0 * (0.85 + 0.5 * (fbm(4, 2) + 0.5))
into = np.clip(s, 0, None) / reach
z_lagoon = 0.8 + 7.5 * smoothstep(0, 0.38, into) + 27.0 * smoothstep(0.22, 1.0, into) ** 1.25
z_lagoon *= 1 + 0.5 * fbm(5, 3)
veins = (1 - np.abs(2 * value_noise(26) - 1)) ** 3
z_lagoon *= 1 - 0.42 * veins * np.clip(1 - s / 28, 0, 1)
w_out = smoothstep(-0.8, 0.9, d)
w_lagoon = smoothstep(0, 4.5, s)
on_flat = (1 - w_out) * (1 - w_lagoon)
z = z_flat * on_flat + z_out * w_out + z_lagoon * w_lagoon

# The water shoals toward each islet.
z = np.minimum(z, 0.2 + 0.5 * np.clip(-land, 0, None) ** 1.4)

# Coral heads in the lagoon, many small and a few large, none round.
heads = np.zeros((S, S), np.float32)
ragged = 1 + 1.4 * fbm(34, 3)
for _ in range(20):
    a = rng.uniform(0, 2 * math.pi)
    room = float(crest(np.array([a]))[0]) - 24
    f = math.sqrt(rng.uniform(0.03, 0.85))
    hx, hy = CX + f * room * math.cos(a), CY + f * room * math.sin(a)
    radius = 1.0 + 5.0 * rng.uniform(0, 1) ** 3
    dist2 = ((XW - hx) ** 2 + (YW - hy) ** 2) * ragged ** 2
    heads = np.maximum(heads, np.exp(-dist2 / radius ** 2) * (0.75 + 0.25 * min(1.0, radius / 3)))
heads *= w_lagoon
mottle = np.clip(0.5 + 1.9 * fbm(95, 3), 0, 1)
z = mix(z, np.minimum(z, 1.2 + 2.4 * mottle), smoothstep(0.18, 0.6, heads))

# The passes: channels through the rim, from the lagoon to the ocean.
in_pass = np.zeros((S, S), np.float32)
for centre, half, slant in PASSES:
    aside = (deg - (centre + slant * d + 2.0 * np.sin(d / 5.0)) + 180.0) % 360.0 - 180.0
    cut = smoothstep(half, half * 0.3, np.abs(aside))
    cut *= np.exp(-(np.clip(s, 0, None) / 13.0) ** 2) * (1 - smoothstep(4, 16, d))
    channel = 9.0 + 6.0 * (1 - np.clip(np.abs(aside) / half, 0, 1) ** 2)
    z = mix(z, np.maximum(z, channel), cut)
    in_pass = np.maximum(in_pass, cut)

# The colours of water over white sand, by depth.
DEPTHS = [0.0, 0.5, 1.5, 3.0, 6.0, 12.0, 20.0, 35.0, 60.0, 120.0, 400.0]
COLOURS = [(226, 244, 226), (190, 236, 220), (128, 222, 208), (72, 204, 198), (33, 176, 192), (22, 142, 180),
           (17, 112, 160), (13, 86, 136), (10, 64, 110), (8, 46, 86), (6, 34, 66)]
zc = np.clip(z, 0, 400)
water = np.stack([np.interp(zc, DEPTHS, [c[i] for c in COLOURS]) for i in range(3)], axis=-1).astype(np.float32)

# The reef flat is alive: brown and olive under the water, in patches, more so toward the crest. The coral heads too.
alive = np.clip(0.5 + 2.0 * fbm(48, 4), 0, 1)
tint = (0.2 + 0.5 * smoothstep(-9.0, 0.0, d)) * alive * on_flat * (1 - in_pass)
tint = np.maximum(tint, 0.36 * smoothstep(0.5, 0.9, heads) * smoothstep(0.4, 0.8, mottle))
water = mix(water, np.array([142, 150, 108], np.float32), tint[..., None])
ridge = np.exp(-((d + 0.6) / 1.1) ** 2) * (1 - in_pass)
water = mix(water, np.array([176, 160, 132], np.float32), (0.35 * ridge)[..., None])

# The swell comes from the east, and breaks on the reef it meets: a torn white line, thin on the lee side.
wind = math.radians(18)
windward = 0.22 + 0.78 * np.clip(0.62 + 0.5 * np.cos(theta - wind), 0, 1) ** 1.3
streaks = np.clip(0.5 + 2.4 * fbm(130, 3), 0, 1)
torn = np.clip(0.5 + 2.2 * fbm(30, 3), 0, 1)
foam = 1.7 * windward * np.exp(-((d - 1.8) / 1.25) ** 2) * (0.3 + 1.0 * streaks) * np.clip(0.2 + 1.5 * torn, 0, 1.3)
foam += 0.9 * windward * np.exp(-((d - 5.2) / 0.8) ** 2) * np.clip(2.8 * streaks * torn - 0.75, 0, 1)
foam += 0.5 * windward * np.exp(-((d + 1.2) / 2.2) ** 2) * np.clip(1.9 * streaks - 0.6, 0, 1)
foam *= 1 - 0.85 * in_pass
foam = np.clip((foam - 0.16) * 1.5, 0, 1) ** 0.8

# The land: a beach, wider toward the lagoon and at the ends of an islet, then the trees.
beach = 0.7 + 2.2 * lagoon_side + 3.2 * np.clip(0.5 + 1.7 * fbm(20, 3), 0, 1) + 3.0 * (1 - smoothstep(1.0, 12.0, to_tip))
grown = land - np.clip(beach, 0.5, None) + 1.1 * fbm(150, 2)
is_land = smoothstep(-0.12, 0.12, land)
is_grown = smoothstep(-0.15, 0.25, grown)
sand = mix(np.array([222, 204, 160], np.float32), np.array([245, 234, 202], np.float32), smoothstep(0, 1.0, land)[..., None])
sand *= (1 + 0.06 * fbm(120, 2))[..., None]

# The trees are lit from the north-west, crown by crown.
crowns = 0.45 * value_noise(170) + 0.35 * value_noise(300) + 0.2 * value_noise(520)
dome = np.clip(blur(np.clip(grown, 0, 3.0), 0.9), 0, None)
canopy = dome ** 0.5 * 0.5 + 1.3 * crowns * is_grown
gy, gx = np.gradient(canopy, 1.0 / PX)
STEEP, HIGH = 0.55, 1.25
lit = (HIGH + STEEP * (gx + gy)) / (np.sqrt(1 + STEEP * STEEP * (gx * gx + gy * gy)) * HIGH)
lit = np.clip(lit, 0.45, 1.5)
tone = np.clip(0.5 + 1.6 * fbm(22, 3), 0, 1)[..., None]
green = mix(np.array([34, 94, 50], np.float32), np.array([102, 160, 70], np.float32), tone)
green = mix(green, np.array([124, 158, 92], np.float32), (0.55 * (1 - smoothstep(0, 1.4, grown)))[..., None])
green = green * (0.6 + 0.8 * crowns)[..., None] * lit[..., None]

# Their shadow falls to the south-east, on the sand and on the shallows.
shift = int(round(1.5 * PX))
shadow = np.roll(np.roll(blur(is_grown, 0.5), shift, axis=0), shift, axis=1)
shadow = np.clip(shadow - is_grown, 0, 1)

picture = mix(water, np.array([252, 253, 250], np.float32), foam[..., None])
picture = mix(picture, sand, is_land[..., None])
picture *= (1 - 0.2 * shadow)[..., None]
picture = mix(picture, green, is_grown[..., None])

# The open sea is never flat: a swell, a little lighter and darker in long lines.
grid = rng.random((int(U / 2.2) + 3, int(U / 14) + 3)).astype(np.float32)
swell = Image.fromarray(grid).resize((int(S * 1.6), int(S * 1.6)), Image.BICUBIC).rotate(-24, Image.BICUBIC)
o = int(S * 0.3)
swell = np.asarray(swell, dtype=np.float32)[o:o + S, o:o + S] - 0.5
deep = smoothstep(5, 40, z) * (1 - is_land)
picture *= (1 + 0.075 * swell * deep + 0.03 * fbm(40, 3) * (1 - is_land))[..., None]

# The lens: a little darker toward the corners, and the grain of a photograph.
far = ((X - 128) ** 2 + (Y - 128) ** 2) / (128.0 ** 2)
picture *= (1 - 0.15 * np.clip(far - 0.25, 0, None))[..., None]
picture += rng.normal(0, 0.9, (S, S, 1)).astype(np.float32)

# The tile: a square with rounded corners.
RADIUS = 56.0
qx = np.abs(X - 128) - (128 - RADIUS)
qy = np.abs(Y - 128) - (128 - RADIUS)
outside = np.hypot(np.clip(qx, 0, None), np.clip(qy, 0, None)) + np.minimum(np.maximum(qx, qy), 0) - RADIUS
alpha = np.clip(0.5 - outside * PX, 0, 1)

pixels = np.dstack([np.clip(picture, 0, 255), alpha * 255]).astype(np.uint8)
Image.fromarray(pixels, 'RGBA').resize((SIZE, SIZE), Image.LANCZOS).save(OUT, optimize=True)
print(OUT)
