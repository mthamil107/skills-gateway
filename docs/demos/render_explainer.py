"""Render the Skills Gateway explainer GIF using Pillow.

A 1080x1080 square explainer that walks a scroll-past viewer through eight
storyboard beats in ~22 seconds, then loops:

  1. Title / pain statement
  2. One skills folder, copied everywhere
  3. Red pain banner sweeps in
  4. The gateway inserts itself in the middle
  5. One request: identity -> policy -> allow/deny
  6. Every file hashed into one version digest
  7. One catalog, three ways out
  8. Hold + CTA caption

Pacing lives in SPEED and BEAT_FRAMES_BASE below, not in the beats.

Reproducible: no randomness, no HTTP, no file IO besides the output GIF.

    python docs/demos/render_explainer.py
"""

from __future__ import annotations

import os
from pathlib import Path

from PIL import Image, ImageDraw, ImageFont

# ---------------------------------------------------------------------------
# Theme - light, "technical book" feel.
# ---------------------------------------------------------------------------

BG = (248, 246, 242)          # cream paper
FG = (26, 26, 26)             # near-black ink
DIM = (110, 110, 110)         # body text grey
SOFT = (210, 206, 198)        # hairline ruled-page grey
TEAL = (22, 105, 122)         # Skills Gateway accent
TEAL_LIGHT = (214, 232, 236)  # accent fill
RED = (200, 65, 65)           # denial / pain
RED_LIGHT = (245, 220, 220)
GREEN = (44, 140, 94)         # allow
GREEN_LIGHT = (218, 238, 228)
AMBER = (176, 122, 30)        # digest / integrity
AMBER_LIGHT = (245, 232, 206)

# The agents a skill has to reach. Desaturated so the gateway's teal stays
# visually dominant from beat 4 on.
AGENTS: list[tuple[str, str, tuple[int, int, int], tuple[int, int, int]]] = [
    ("Claude Code", ".claude/skills", (215, 200, 230), (90, 60, 130)),
    ("Codex", ".agents/skills", (205, 222, 230), (40, 90, 110)),
    ("Cursor", ".cursor/skills", (235, 215, 195), (140, 80, 30)),
    ("Copilot", ".github/skills", (215, 230, 205), (60, 110, 50)),
    ("Gemini CLI", ".gemini/skills", (230, 220, 200), (130, 100, 40)),
]

# ---------------------------------------------------------------------------
# Geometry - 1080x1080 square.
# ---------------------------------------------------------------------------

WIDTH = 1080
HEIGHT = 1080
FPS = 12
FRAME_MS = int(round(1000 / FPS))

# SPEED stretches every hold and every stagger. 1.0 is brisk; raise it until
# a first-time reader can finish the densest beat without pausing. Held
# frames are pixel-identical, so they merge on export: a slower GIF costs
# almost nothing in bytes.
SPEED = 1.8

# Base frames per beat at SPEED 1.0, where 12 frames = 1.0s.
BEAT_FRAMES_BASE = [
    14,   # B1 title
    18,   # B2 copied everywhere
    16,   # B3 pain banner
    14,   # B4 gateway inserts
    24,   # B5 one request
    20,   # B6 digest
    20,   # B7 three ways out
    20,   # B8 hold + CTA
]
BEAT_FRAMES = [int(round(b * SPEED)) for b in BEAT_FRAMES_BASE]
TOTAL_FRAMES = sum(BEAT_FRAMES)

EASE_FRAMES = 3

FONT_CANDIDATES_REGULAR = [
    r"C:\Windows\Fonts\segoeui.ttf",
    r"C:\Windows\Fonts\arial.ttf",
    "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
    "/Library/Fonts/Arial.ttf",
]
FONT_CANDIDATES_BOLD = [
    r"C:\Windows\Fonts\segoeuib.ttf",
    r"C:\Windows\Fonts\arialbd.ttf",
    "/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf",
    "/Library/Fonts/Arial Bold.ttf",
]
FONT_CANDIDATES_MONO = [
    r"C:\Windows\Fonts\CascadiaMono.ttf",
    r"C:\Windows\Fonts\consola.ttf",
    "/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf",
]


def _load_font(candidates: list[str], size: int) -> ImageFont.ImageFont:
    for p in candidates:
        try:
            if os.path.exists(p):
                return ImageFont.truetype(p, size)
        except OSError:
            continue
    return ImageFont.load_default()


def _build_fonts() -> dict[str, ImageFont.ImageFont]:
    return {
        "h1": _load_font(FONT_CANDIDATES_BOLD, 62),
        "h2": _load_font(FONT_CANDIDATES_BOLD, 44),
        "h3": _load_font(FONT_CANDIDATES_BOLD, 34),
        "body": _load_font(FONT_CANDIDATES_REGULAR, 30),
        "small": _load_font(FONT_CANDIDATES_REGULAR, 25),
        "tiny": _load_font(FONT_CANDIDATES_REGULAR, 21),
        "label": _load_font(FONT_CANDIDATES_BOLD, 26),
        "mono": _load_font(FONT_CANDIDATES_MONO, 25),
        "mono_small": _load_font(FONT_CANDIDATES_MONO, 21),
        "step": _load_font(FONT_CANDIDATES_BOLD, 30),
    }


# ---------------------------------------------------------------------------
# Easing
# ---------------------------------------------------------------------------

def ease_out_cubic(t: float) -> float:
    t = max(0.0, min(1.0, t))
    return 1.0 - (1.0 - t) ** 3


def pop(frame_in_beat: int, delay: int = 0, ease: int = EASE_FRAMES) -> float:
    """0..1 progress for a pop-in that starts `delay` frames into the beat.

    Delay and ease both scale with SPEED, so the choreography written at
    SPEED 1.0 keeps its shape when the whole GIF is slowed down.
    """
    if ease <= 0:
        return 1.0
    return ease_out_cubic((frame_in_beat - delay * SPEED) / (ease * SPEED))


def blend(a: tuple[int, int, int], b: tuple[int, int, int], t: float) -> tuple[int, int, int]:
    t = max(0.0, min(1.0, t))
    return tuple(int(round(a[i] + (b[i] - a[i]) * t)) for i in range(3))  # type: ignore[return-value]


# ---------------------------------------------------------------------------
# Drawing primitives
# ---------------------------------------------------------------------------

def text_center(d, text, cx, cy, font, fill=FG) -> None:
    bbox = d.textbbox((0, 0), text, font=font)
    w, h = bbox[2] - bbox[0], bbox[3] - bbox[1]
    d.text((cx - w / 2 - bbox[0], cy - h / 2 - bbox[1]), text, fill=fill, font=font)


def text_at(d, text, x, y, font, fill=FG, anchor: str = "lt") -> None:
    bbox = d.textbbox((0, 0), text, font=font)
    w, h = bbox[2] - bbox[0], bbox[3] - bbox[1]
    dx = -bbox[0] if anchor[0] == "l" else (-bbox[2] if anchor[0] == "r" else -w / 2 - bbox[0])
    dy = -bbox[1] if anchor[1] == "t" else (-bbox[3] if anchor[1] == "b" else -h / 2 - bbox[1])
    d.text((x + dx, y + dy), text, fill=fill, font=font)


def text_width(d, text, font) -> int:
    bbox = d.textbbox((0, 0), text, font=font)
    return bbox[2] - bbox[0]


def box(d, rect, radius=18, fill=None, outline=None, width=3) -> None:
    d.rounded_rectangle(rect, radius=radius, fill=fill, outline=outline, width=width)


def arrow(d, x1, y1, x2, y2, color, width=4, head=14) -> None:
    d.line((x1, y1, x2, y2), fill=color, width=width)
    dx, dy = x2 - x1, y2 - y1
    length = (dx * dx + dy * dy) ** 0.5 or 1.0
    ux, uy = dx / length, dy / length
    px, py = -uy, ux
    d.polygon(
        [
            (x2, y2),
            (x2 - ux * head + px * head * 0.55, y2 - uy * head + py * head * 0.55),
            (x2 - ux * head - px * head * 0.55, y2 - uy * head - py * head * 0.55),
        ],
        fill=color,
    )


def pill(d, cx, cy, label, font, fg, bg, pad_x=18, pad_y=10) -> tuple[int, int, int, int]:
    w = text_width(d, label, font)
    bbox = d.textbbox((0, 0), label, font=font)
    h = bbox[3] - bbox[1]
    rect = (cx - w / 2 - pad_x, cy - h / 2 - pad_y, cx + w / 2 + pad_x, cy + h / 2 + pad_y)
    d.rounded_rectangle(rect, radius=int((rect[3] - rect[1]) / 2), fill=bg, outline=fg, width=2)
    text_center(d, label, cx, cy, font, fg)
    return rect  # type: ignore[return-value]


def page_frame(d, fonts, caption: str | None = None) -> None:
    """The ruled-page border every beat shares, plus an optional caption."""
    d.rounded_rectangle((28, 28, WIDTH - 28, HEIGHT - 28), radius=26, outline=SOFT, width=3)
    text_at(d, "skills-gateway", 58, 58, fonts["tiny"], DIM)
    if caption:
        text_center(d, caption, WIDTH // 2, HEIGHT - 66, fonts["small"], DIM)


def skill_card(d, fonts, rect, scale=1.0, title="code-review", show_files=True) -> None:
    """A SKILL.md folder card."""
    if scale < 1.0:
        cx, cy = (rect[0] + rect[2]) / 2, (rect[1] + rect[3]) / 2
        hw, hh = (rect[2] - rect[0]) / 2 * scale, (rect[3] - rect[1]) / 2 * scale
        rect = (cx - hw, cy - hh, cx + hw, cy + hh)
    box(d, rect, radius=16, fill=(255, 254, 251), outline=FG, width=3)
    x, y = rect[0] + 22, rect[1] + 20
    text_at(d, "SKILL.md", x, y, fonts["mono"], FG)
    if show_files and (rect[3] - rect[1]) > 120:
        text_at(d, "name: " + title, x, y + 40, fonts["mono_small"], DIM)
        text_at(d, "references/", x, y + 70, fonts["mono_small"], DIM)
        text_at(d, "scripts/", x, y + 98, fonts["mono_small"], DIM)


def gateway_box(d, fonts, rect, t=1.0, show_rows=True) -> None:
    """The gateway: teal box with its three responsibilities."""
    fill = blend(BG, TEAL_LIGHT, t)
    box(d, rect, radius=22, fill=fill, outline=TEAL, width=4)
    cx = (rect[0] + rect[2]) / 2
    text_center(d, "Skills Gateway", cx, rect[1] + 42, fonts["h3"], TEAL)
    if not show_rows:
        return
    rows = ["identity", "policy", "audit"]
    y = rect[1] + 92
    for i, r in enumerate(rows):
        if t < 0.35 + i * 0.2:
            continue
        text_center(d, r, cx, y + i * 38, fonts["small"], TEAL)


# ---------------------------------------------------------------------------
# Beat 1 - title
# ---------------------------------------------------------------------------

def beat1(d, fonts, f: int) -> None:
    page_frame(d, fonts)
    t = pop(f, 0)
    text_center(d, "Skills are easy to write.", WIDTH // 2, 380, fonts["h1"], blend(BG, FG, t))
    t2 = pop(f, 3)
    text_center(d, "Handing them out is not.", WIDTH // 2, 470, fonts["h1"], blend(BG, RED, t2))
    t3 = pop(f, 7)
    if t3 > 0:
        text_center(
            d,
            "one skill folder  ->  every agent, every laptop, every CI job",
            WIDTH // 2,
            580,
            fonts["body"],
            blend(BG, DIM, t3),
        )
    t4 = pop(f, 10)
    if t4 > 0:
        pill(d, WIDTH // 2, 680, "SKILL.md  ·  the open standard", fonts["label"], TEAL, blend(BG, TEAL_LIGHT, t4))


# ---------------------------------------------------------------------------
# Beat 2 - copied everywhere
# ---------------------------------------------------------------------------

def _agent_targets(d, fonts, f: int, delay0: int, dim: bool = False) -> list[tuple[int, int]]:
    """Draw the five agent destination cards down the right side."""
    centers = []
    top = 210
    gap = 128
    for i, (name, path, fill, accent) in enumerate(AGENTS):
        t = pop(f, delay0 + i)
        if t <= 0:
            centers.append((800, top + i * gap + 42))
            continue
        rect = (664, top + i * gap, 1024, top + i * gap + 96)
        f_fill = blend(BG, fill, t) if not dim else blend(BG, fill, 0.45 * t)
        f_line = blend(BG, accent, t) if not dim else blend(BG, accent, 0.5 * t)
        box(d, rect, radius=14, fill=f_fill, outline=f_line, width=3)
        text_at(d, name, rect[0] + 24, rect[1] + 26, fonts["label"], f_line)
        text_at(d, path, rect[0] + 24, rect[1] + 60, fonts["mono_small"], blend(BG, DIM, t))
        centers.append((rect[0], (rect[1] + rect[3]) / 2))
    return centers


def beat2(d, fonts, f: int) -> None:
    page_frame(d, fonts, "today: copy, symlink, paste - and hope")
    text_center(d, "One folder, copied by hand", WIDTH // 2, 132, fonts["h2"], FG)

    skill_card(d, fonts, (70, 380, 400, 560))
    text_at(d, "your skills repo", 70, 348, fonts["small"], DIM)

    centers = _agent_targets(d, fonts, f, delay0=2)
    for i, (tx, ty) in enumerate(centers):
        t = pop(f, 4 + i)
        if t <= 0.05:
            continue
        x1, y1 = 404, 470
        x2 = x1 + (tx - x1) * t
        y2 = y1 + (ty - y1) * t
        arrow(d, x1, y1, int(x2), int(y2), blend(BG, DIM, 0.8 * t), width=3, head=11)

    t = pop(f, 11)
    if t > 0:
        for i, line in enumerate(
            ["copies drift", "no record of who has what", "no way to keep one team out"]
        ):
            text_at(d, "·  " + line, 70, 640 + i * 42, fonts["small"], blend(BG, DIM, t))


# ---------------------------------------------------------------------------
# Beat 3 - pain banner
# ---------------------------------------------------------------------------

def beat3(d, fonts, f: int) -> None:
    page_frame(d, fonts, "Snyk ToxicSkills audit, February 2026")
    text_center(d, "And a skill is not just a file", WIDTH // 2, 132, fonts["h2"], FG)

    t = ease_out_cubic(min(1.0, f / (5 * SPEED)))
    bw, bh = 860, 260
    bx = int(WIDTH / 2 - bw / 2)
    by = int(420 - bh / 2)
    slide = int((1 - t) * -WIDTH)
    rect = (bx + slide, by, bx + bw + slide, by + bh)
    box(d, rect, radius=20, fill=RED_LIGHT, outline=RED, width=4)
    cx = (rect[0] + rect[2]) / 2
    text_center(d, "36.8%", cx, rect[1] + 80, fonts["h1"], RED)
    text_center(d, "of 3,984 public skills carried", cx, rect[1] + 158, fonts["body"], FG)
    text_center(d, "at least one security flaw", cx, rect[1] + 200, fonts["body"], FG)

    t2 = pop(f, 7)
    if t2 > 0:
        text_center(
            d,
            "a skill is instructions your agent will follow",
            WIDTH // 2,
            660,
            fonts["h3"],
            blend(BG, FG, t2),
        )
        text_center(
            d,
            "so distribution is a supply chain",
            WIDTH // 2,
            716,
            fonts["h3"],
            blend(BG, RED, pop(f, 10)),
        )


# ---------------------------------------------------------------------------
# Beat 4 - the gateway inserts itself
# ---------------------------------------------------------------------------

def beat4(d, fonts, f: int) -> None:
    page_frame(d, fonts, "publish once - the gateway hands it out")
    text_center(d, "Put one governed hop in the middle", WIDTH // 2, 132, fonts["h2"], FG)

    skill_card(d, fonts, (40, 390, 316, 550))
    text_at(d, "your skills repo", 40, 358, fonts["small"], DIM)

    t = ease_out_cubic(min(1.0, f / (6 * SPEED)))
    gw = (int(346 - (1 - t) * 40), 330, int(620 - (1 - t) * 40), 610)
    gateway_box(d, fonts, gw, t=t)

    centers = _agent_targets(d, fonts, 99, delay0=0)
    arrow(d, 322, 470, gw[0] - 8, 470, TEAL, width=4)
    for i, (tx, ty) in enumerate(centers):
        tt = pop(f, 4 + i)
        if tt <= 0.05:
            continue
        x1, y1 = gw[2] + 6, 470
        arrow(d, x1, y1, int(x1 + (tx - x1) * tt), int(y1 + (ty - y1) * tt), TEAL, width=3, head=11)

    t2 = pop(f, 10)
    if t2 > 0:
        text_center(
            d,
            "one binary  ·  SQLite  ·  no platform to run",
            WIDTH // 2,
            905,
            fonts["body"],
            blend(BG, DIM, t2),
        )


# ---------------------------------------------------------------------------
# Beat 5 - one request
# ---------------------------------------------------------------------------

def beat5(d, fonts, f: int) -> None:
    page_frame(d, fonts, "denied looks exactly like missing - the policy leaks nothing")
    text_center(d, "Every single fetch is decided", WIDTH // 2, 120, fonts["h2"], FG)

    # The request token.
    t0 = pop(f, 0)
    if t0 > 0:
        pill(
            d,
            WIDTH // 2,
            210,
            "GET  platform/code-review   Bearer <token>",
            fonts["mono"],
            blend(BG, FG, t0),
            blend(BG, (255, 254, 251), t0),
        )

    steps = [
        ("1", "identity", "subject · teams · roles · agent type, read from the token"),
        ("2", "policy", "default deny, a deny always wins · namespace · name · version"),
        ("3", "version", "published versions never change · latest = newest THEY may fetch"),
    ]
    y = 300
    for i, (num, title, line1) in enumerate(steps):
        t = pop(f, 2 + i * 2)
        if t <= 0.05:
            continue
        rect = (150, y + i * 132, 930, y + i * 132 + 108)
        box(d, rect, radius=16, fill=blend(BG, TEAL_LIGHT, 0.55 * t), outline=blend(BG, TEAL, t), width=3)
        text_center(d, num, rect[0] + 44, (rect[1] + rect[3]) / 2, fonts["step"], blend(BG, TEAL, t))
        text_at(d, title, rect[0] + 88, rect[1] + 22, fonts["label"], blend(BG, FG, t))
        text_at(d, line1, rect[0] + 88, rect[1] + 60, fonts["small"], blend(BG, DIM, t))
        if i < 2 and t > 0.8:
            arrow(d, 540, rect[3] + 4, 540, rect[3] + 22, TEAL, width=3, head=10)

    # The two outcomes.
    t_out = pop(f, 9)
    if t_out > 0:
        arrow(d, 540, 700, 360, 752, blend(BG, GREEN, t_out), width=3)
        arrow(d, 540, 700, 720, 752, blend(BG, RED, t_out), width=3)
        box(d, (150, 760, 500, 872), radius=16, fill=blend(BG, GREEN_LIGHT, t_out), outline=blend(BG, GREEN, t_out), width=3)
        text_center(d, "allowed", 325, 798, fonts["label"], blend(BG, GREEN, t_out))
        text_center(d, "the skill, verified", 325, 840, fonts["small"], blend(BG, FG, t_out))

        t_deny = pop(f, 12)
        box(d, (580, 760, 930, 872), radius=16, fill=blend(BG, RED_LIGHT, t_deny), outline=blend(BG, RED, t_deny), width=3)
        text_center(d, "404 not found", 755, 798, fonts["label"], blend(BG, RED, t_deny))
        text_center(d, "same as a skill that isn't there", 755, 840, fonts["small"], blend(BG, FG, t_deny))

    t_audit = pop(f, 15)
    if t_audit > 0:
        text_center(
            d,
            "either way, one append-only audit row: who · what · when",
            WIDTH // 2,
            920,
            fonts["small"],
            blend(BG, DIM, t_audit),
        )


# ---------------------------------------------------------------------------
# Beat 6 - digest
# ---------------------------------------------------------------------------

def beat6(d, fonts, f: int) -> None:
    page_frame(d, fonts, "re-zip it anywhere: same files, same digest")
    text_center(d, "A version is frozen and fingerprinted", WIDTH // 2, 130, fonts["h2"], FG)

    files = [
        ("SKILL.md", "599662b6"),
        ("references/checklist.md", "d7dff65b"),
        ("scripts/run.sh", "4c1ab902"),
    ]
    box(d, (90, 250, 520, 470), radius=16, fill=(255, 254, 251), outline=FG, width=3)
    text_at(d, "the skill folder", 108, 268, fonts["small"], DIM)
    for i, (name, _) in enumerate(files):
        text_at(d, name, 112, 318 + i * 44, fonts["mono_small"], FG)

    for i, (_, h) in enumerate(files):
        t = pop(f, 1 + i)
        if t <= 0.05:
            continue
        y = 330 + i * 44
        arrow(d, 530, y, int(530 + 120 * t), y, blend(BG, AMBER, t), width=3, head=10)
        if t > 0.6:
            text_at(d, "sha256  " + h + "...", 670, y, fonts["mono_small"], blend(BG, AMBER, t), anchor="lm")

    t4 = pop(f, 6)
    if t4 > 0:
        text_center(
            d, "every file hashed, sorted, hashed again", WIDTH // 2, 520, fonts["small"], blend(BG, DIM, t4)
        )
        arrow(d, WIDTH // 2, 548, WIDTH // 2, 588, blend(BG, AMBER, t4), width=3)

    t5 = pop(f, 8)
    if t5 > 0:
        box(d, (150, 600, 930, 700), radius=18, fill=blend(BG, AMBER_LIGHT, t5), outline=blend(BG, AMBER, t5), width=4)
        text_center(d, "sha256:344413734f917ab6...", WIDTH // 2, 650, fonts["mono"], blend(BG, FG, t5))

    t6 = pop(f, 11)
    if t6 > 0:
        for i, line in enumerate(
            [
                "published once - it can never be overwritten",
                "sent on every download - the client re-checks it",
                "pinned in sgw-lock.json - commit it with your code",
            ]
        ):
            text_at(d, "·  " + line, 150, 750 + i * 44, fonts["small"], blend(BG, FG, pop(f, 11 + i)))


# ---------------------------------------------------------------------------
# Beat 7 - three ways out
# ---------------------------------------------------------------------------

def beat7(d, fonts, f: int) -> None:
    page_frame(d, fonts, "MCP: the Skills Extension, SEP-2640")
    text_center(d, "One governed catalog, three ways out", WIDTH // 2, 130, fonts["h2"], FG)

    gw = (390, 220, 690, 330)
    gateway_box(d, fonts, gw, t=1.0, show_rows=False)

    lanes = [
        ("REST  /v1", ["publish, fetch,", "bundle + manifest", "for CI and scripts"], TEAL),
        ("sgw sync", ["writes .claude/skills,", ".cursor/skills, ...", "+ a digest lock file"], GREEN),
        ("MCP  /mcp", ["skills/list, skills/get", "over skill:// URIs", "+ tools for today's clients"], AMBER),
    ]
    xs = [90, 396, 702]
    for i, (title, lines, color) in enumerate(lanes):
        t = pop(f, 1 + i * 2)
        if t <= 0.05:
            continue
        rect = (xs[i], 430, xs[i] + 288, 700)
        arrow(d, (gw[0] + gw[2]) / 2, gw[3] + 6, xs[i] + 144, 424, blend(BG, color, t), width=3, head=12)
        box(d, rect, radius=18, fill=blend(BG, (255, 254, 251), t), outline=blend(BG, color, t), width=3)
        text_center(d, title, xs[i] + 144, rect[1] + 44, fonts["label"], blend(BG, color, t))
        for j, line in enumerate(lines):
            text_center(d, line, xs[i] + 144, rect[1] + 108 + j * 42, fonts["small"], blend(BG, DIM, t))

    t2 = pop(f, 9)
    if t2 > 0:
        text_center(
            d,
            "the same identity and the same policy behind all three",
            WIDTH // 2,
            770,
            fonts["h3"],
            blend(BG, FG, t2),
        )
    t3 = pop(f, 12)
    if t3 > 0:
        text_center(
            d,
            "an agent sees only what its user may load",
            WIDTH // 2,
            828,
            fonts["body"],
            blend(BG, DIM, t3),
        )


# ---------------------------------------------------------------------------
# Beat 8 - hold + CTA
# ---------------------------------------------------------------------------

def beat8(d, fonts, f: int) -> None:
    page_frame(d, fonts)
    t = pop(f, 0)
    text_center(d, "Skills Gateway", WIDTH // 2, 300, fonts["h1"], blend(BG, TEAL, t))
    text_center(
        d,
        "governed, identity-aware distribution for Agent Skills",
        WIDTH // 2,
        378,
        fonts["body"],
        blend(BG, DIM, pop(f, 2)),
    )

    rows = [
        ("OIDC identity + default-deny policy", "on every single fetch", TEAL),
        ("immutable, digest-verified versions", "end to end, with a lock file", AMBER),
        ("append-only audit", "who loaded what, and when", GREEN),
        ("REST · native sync · MCP SEP-2640", "one catalog, every agent", TEAL),
    ]
    for i, (left, right, color) in enumerate(rows):
        tt = pop(f, 4 + i)
        if tt <= 0.05:
            continue
        y = 470 + i * 78
        d.ellipse((150, y - 8, 166, y + 8), fill=blend(BG, color, tt))
        text_at(d, left, 196, y, fonts["label"], blend(BG, FG, tt), anchor="lm")
        text_at(d, right, 930, y, fonts["small"], blend(BG, DIM, tt), anchor="rm")

    t2 = pop(f, 10)
    if t2 > 0:
        pill(
            d,
            WIDTH // 2,
            840,
            "github.com/mthamil107/skills-gateway",
            fonts["label"],
            blend(BG, TEAL, t2),
            blend(BG, TEAL_LIGHT, t2),
            pad_x=26,
            pad_y=14,
        )
    t3 = pop(f, 13)
    if t3 > 0:
        text_center(d, "Apache-2.0  ·  one Go binary", WIDTH // 2, 916, fonts["small"], blend(BG, DIM, t3))


BEATS = [beat1, beat2, beat3, beat4, beat5, beat6, beat7, beat8]


# ---------------------------------------------------------------------------
# Assembly
# ---------------------------------------------------------------------------

def render_frame(index: int, fonts) -> Image.Image:
    img = Image.new("RGB", (WIDTH, HEIGHT), BG)
    d = ImageDraw.Draw(img)
    start = 0
    for beat_i, length in enumerate(BEAT_FRAMES):
        if index < start + length:
            BEATS[beat_i](d, fonts, index - start)
            return img
        start += length
    BEATS[-1](d, fonts, BEAT_FRAMES[-1] - 1)
    return img


def render(out_path: Path | None = None) -> Path:
    out_path = out_path or Path(__file__).with_name("skills-gateway-explainer.gif")
    fonts = _build_fonts()
    frames = [render_frame(i, fonts) for i in range(TOTAL_FRAMES)]
    durations = [FRAME_MS] * TOTAL_FRAMES

    # Build the shared palette from one settled frame per beat, so colours do
    # not shift between beats.
    sample_indices = []
    start = 0
    for length in BEAT_FRAMES:
        sample_indices.append(min(TOTAL_FRAMES - 1, start + length - 1))
        start += length
    strip = Image.new("RGB", (WIDTH, HEIGHT * len(sample_indices)), BG)
    for i, fi in enumerate(sample_indices):
        strip.paste(frames[fi], (0, i * HEIGHT))
    palette_source = strip.quantize(colors=64, method=Image.Quantize.MEDIANCUT, dither=Image.Dither.NONE)

    quant = [f.quantize(palette=palette_source, dither=Image.Dither.NONE) for f in frames]

    # Pillow drops the duration of consecutive pixel-identical frames, which
    # would shorten the loop. Collapse such runs and keep their time.
    merged: list[Image.Image] = []
    merged_durations: list[int] = []
    prev: bytes | None = None
    for qf, dur in zip(quant, durations):
        cur = qf.tobytes()
        if prev is not None and cur == prev:
            merged_durations[-1] += dur
            continue
        merged.append(qf)
        merged_durations.append(dur)
        prev = cur

    merged[0].save(
        out_path,
        save_all=True,
        append_images=merged[1:],
        duration=merged_durations,
        loop=0,
        optimize=True,
        disposal=1,
    )
    size = out_path.stat().st_size
    print(
        f"wrote {out_path} ({size / 1024:.1f} KB, {len(merged)} stored / {len(frames)} logical frames, "
        f"{sum(merged_durations) / 1000:.1f}s @ {FPS} fps)"
    )
    return out_path


if __name__ == "__main__":
    render()
