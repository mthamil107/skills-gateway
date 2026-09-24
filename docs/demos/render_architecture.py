"""Render the Skills Gateway architecture diagram (a 16:9 slide) with Pillow.

One picture that answers four questions at a glance:

  * where skills come from   - a folder, a git repo, or a publish
  * what the gateway does    - identity, policy, registry, audit, store
  * how they reach an agent  - REST, native sync, MCP (SEP-2640)
  * what is promised         - digests end to end, immutable versions,
                               denied looks like missing, one audit row

Deterministic: no randomness, no network, no inputs.

    python docs/demos/render_architecture.py
"""

from __future__ import annotations

import os
from pathlib import Path

from PIL import Image, ImageDraw, ImageFont

# ---------------------------------------------------------------------------
# Theme - the same "technical book" palette as the explainer GIF.
# ---------------------------------------------------------------------------

BG = (248, 246, 242)
FG = (26, 26, 26)
DIM = (110, 110, 110)
SOFT = (210, 206, 198)
PAPER = (255, 254, 251)
TEAL = (22, 105, 122)
TEAL_PALE = (214, 232, 236)
GREEN = (44, 140, 94)
GREEN_PALE = (218, 238, 228)
AMBER = (176, 122, 30)
AMBER_PALE = (245, 232, 206)
RED = (200, 65, 65)
RED_PALE = (245, 220, 220)
VIOLET = (90, 60, 130)
VIOLET_PALE = (226, 216, 238)

WIDTH, HEIGHT = 2000, 1260

FONTS_REGULAR = [
    r"C:\Windows\Fonts\segoeui.ttf",
    r"C:\Windows\Fonts\arial.ttf",
    "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
]
FONTS_BOLD = [
    r"C:\Windows\Fonts\segoeuib.ttf",
    r"C:\Windows\Fonts\arialbd.ttf",
    "/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf",
]
FONTS_MONO = [
    r"C:\Windows\Fonts\CascadiaMono.ttf",
    r"C:\Windows\Fonts\consola.ttf",
    "/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf",
]


def _font(candidates: list[str], size: int) -> ImageFont.ImageFont:
    for p in candidates:
        try:
            if os.path.exists(p):
                return ImageFont.truetype(p, size)
        except OSError:
            continue
    return ImageFont.load_default()


def build_fonts() -> dict[str, ImageFont.ImageFont]:
    return {
        "title": _font(FONTS_BOLD, 46),
        "sub": _font(FONTS_REGULAR, 26),
        "band": _font(FONTS_BOLD, 21),
        "h": _font(FONTS_BOLD, 29),
        "label": _font(FONTS_BOLD, 23),
        "body": _font(FONTS_REGULAR, 21),
        "small": _font(FONTS_REGULAR, 19),
        "mono": _font(FONTS_MONO, 19),
        "mono_s": _font(FONTS_MONO, 17),
        "num": _font(FONTS_BOLD, 21),
    }


# ---------------------------------------------------------------------------
# Primitives
# ---------------------------------------------------------------------------

def tc(d, text, cx, cy, font, fill=FG) -> None:
    b = d.textbbox((0, 0), text, font=font)
    d.text((cx - (b[2] - b[0]) / 2 - b[0], cy - (b[3] - b[1]) / 2 - b[1]), text, fill=fill, font=font)


def ta(d, text, x, y, font, fill=FG, anchor="lt") -> None:
    b = d.textbbox((0, 0), text, font=font)
    w, h = b[2] - b[0], b[3] - b[1]
    dx = -b[0] if anchor[0] == "l" else (-b[2] if anchor[0] == "r" else -w / 2 - b[0])
    dy = -b[1] if anchor[1] == "t" else (-b[3] if anchor[1] == "b" else -h / 2 - b[1])
    d.text((x + dx, y + dy), text, fill=fill, font=font)


def tw(d, text, font) -> int:
    b = d.textbbox((0, 0), text, font=font)
    return b[2] - b[0]


def box(d, rect, radius=14, fill=None, outline=None, width=2) -> None:
    d.rounded_rectangle(rect, radius=radius, fill=fill, outline=outline, width=width)


def dashed(d, rect, color, dash=14, gap=10, width=2, radius=0) -> None:
    x0, y0, x1, y1 = rect
    for x in range(int(x0), int(x1), dash + gap):
        d.line((x, y0, min(x + dash, x1), y0), fill=color, width=width)
        d.line((x, y1, min(x + dash, x1), y1), fill=color, width=width)
    for y in range(int(y0), int(y1), dash + gap):
        d.line((x0, y, x0, min(y + dash, y1)), fill=color, width=width)
        d.line((x1, y, x1, min(y + dash, y1)), fill=color, width=width)


def arrow(d, x1, y1, x2, y2, color, width=3, head=13, dash=False) -> None:
    if dash:
        n = max(1, int(((x2 - x1) ** 2 + (y2 - y1) ** 2) ** 0.5 // 18))
        for i in range(n):
            a, b = i / n, min(1.0, (i + 0.55) / n)
            d.line((x1 + (x2 - x1) * a, y1 + (y2 - y1) * a, x1 + (x2 - x1) * b, y1 + (y2 - y1) * b),
                   fill=color, width=width)
    else:
        d.line((x1, y1, x2, y2), fill=color, width=width)
    dx, dy = x2 - x1, y2 - y1
    ln = (dx * dx + dy * dy) ** 0.5 or 1.0
    ux, uy = dx / ln, dy / ln
    px, py = -uy, ux
    d.polygon([(x2, y2),
               (x2 - ux * head + px * head * 0.55, y2 - uy * head + py * head * 0.55),
               (x2 - ux * head - px * head * 0.55, y2 - uy * head - py * head * 0.55)], fill=color)


def step(d, n, x, y, fonts, color=TEAL, r=17) -> None:
    """A numbered bullet that ties the picture to the caption strip."""
    d.ellipse((x - r, y - r, x + r, y + r), fill=color)
    tc(d, str(n), x, y, fonts["num"], PAPER)


def card(d, rect, fonts, title, lines, accent, pale, mono=False, title_font="label") -> None:
    box(d, rect, radius=14, fill=pale, outline=accent, width=2)
    cx = (rect[0] + rect[2]) / 2
    tc(d, title, cx, rect[1] + 30, fonts[title_font], accent)
    for i, line in enumerate(lines):
        tc(d, line, cx, rect[1] + 66 + i * 26, fonts["mono_s" if mono else "small"], DIM)


# ---------------------------------------------------------------------------
# The diagram
# ---------------------------------------------------------------------------

def render(out_path: Path | None = None) -> Path:
    out_path = out_path or Path(__file__).with_name("architecture.png")
    fonts = build_fonts()
    img = Image.new("RGB", (WIDTH, HEIGHT), BG)
    d = ImageDraw.Draw(img)

    # Title -----------------------------------------------------------------
    ta(d, "Skills Gateway", 60, 46, fonts["title"], FG)
    ta(d, "governed distribution for Agent Skills (SKILL.md) — one source, every agent, verified end to end",
       60, 104, fonts["sub"], DIM)
    ta(d, "Apache-2.0  ·  one Go binary  ·  SQLite", WIDTH - 60, 60, fonts["small"], DIM, anchor="rt")
    ta(d, "github.com/mthamil107/skills-gateway", WIDTH - 60, 88, fonts["mono_s"], TEAL, anchor="rt")
    d.line((60, 140, WIDTH - 60, 140), fill=SOFT, width=2)

    # Band headings ---------------------------------------------------------
    bands = [(60, 520, "WHERE SKILLS COME FROM"), (600, 1290, "WHAT THE GATEWAY DOES"), (1370, 1940, "HOW AN AGENT GETS THEM")]
    for x0, x1, label in bands:
        ta(d, label, x0, 168, fonts["band"], DIM)
        d.line((x0, 196, x1, 196), fill=SOFT, width=1)

    # ---- Column 1: sources ------------------------------------------------
    card(d, (60, 220, 520, 336), fonts, "a folder", ["path: ../team-skills", "a checkout of your skills repo"], DIM, PAPER)
    card(d, (60, 356, 520, 472), fonts, "a git repository", ["git: github.com/acme/skills", "ref: a branch, tag or commit"], DIM, PAPER)
    card(d, (60, 492, 520, 636), fonts, "published to the gateway", ["sgw publish <dir> -ns platform", "-version 1.4.0", "immutable once published"], TEAL, TEAL_PALE)
    # A skill is a folder, not a file.
    box(d, (60, 700, 520, 900), radius=14, fill=PAPER, outline=FG, width=2)
    tc(d, "a skill is a folder", 290, 730, fonts["label"], FG)
    for i, (name, note) in enumerate([("SKILL.md", "name + description"),
                                      ("references/", "what the skill cites"),
                                      ("scripts/", "what it runs")]):
        ta(d, name, 92, 768 + i * 40, fonts["mono"], FG)
        ta(d, note, 496, 770 + i * 40, fonts["small"], DIM, anchor="rt")

    ta(d, "512 files · 16 MiB per skill · regular files only", 290, 918, fonts["small"], DIM, anchor="mt")
    ta(d, "no links, no absolute paths, no \"..\"", 290, 944, fonts["small"], DIM, anchor="mt")

    # ---- Column 2: the gateway -------------------------------------------
    gw = (600, 220, 1290, 900)
    box(d, gw, radius=20, fill=PAPER, outline=TEAL, width=3)
    tc(d, "Skills Gateway", 945, 258, fonts["h"], TEAL)
    tc(d, "one binary · no platform to run", 945, 292, fonts["small"], DIM)

    rows = [
        (330, "1  IDENTITY", "OIDC: Keycloak · Entra · Okta · Auth0 · Cognito",
         "subject · teams · roles · agent type, read from the token", TEAL, TEAL_PALE),
        (448, "2  POLICY", "default deny · a deny always wins",
         "namespace · name · version range · agent type", TEAL, TEAL_PALE),
        (566, "3  REGISTRY", "immutable versions · SHA-256 per file",
         "\"latest\" = newest version THIS caller may fetch", AMBER, AMBER_PALE),
        (684, "4  AUDIT", "append-only, in the same transaction",
         "who · what · when · from where → JSON Lines", GREEN, GREEN_PALE),
    ]
    for y, title, line1, line2, accent, pale in rows:
        r = (628, y, 1262, y + 100)
        box(d, r, radius=12, fill=pale, outline=accent, width=2)
        ta(d, title, 652, y + 22, fonts["label"], accent)
        ta(d, line1, 652, y + 54, fonts["small"], FG)
        ta(d, line2, 652, y + 78, fonts["small"], DIM)
        if y != 684:
            arrow(d, 945, y + 100, 945, y + 116, TEAL, width=2, head=9)

    box(d, (628, 806, 1262, 872), radius=12, fill=BG, outline=SOFT, width=2)
    tc(d, "SQLite  ·  versions, files, policy decisions, audit", 945, 828, fonts["small"], DIM)
    tc(d, "Postgres and OPA/Cedar are planned, not shipped", 945, 854, fonts["mono_s"], DIM)

    # ---- Column 3: delivery ----------------------------------------------
    surfaces = [
        (220, "REST  /v1", ["publish · fetch · bundle", "manifest · audit export", "for CI and scripts"], TEAL, TEAL_PALE),
        (410, "sgw sync", ["writes each agent's folders", "verifies every digest", "+ sgw-lock.json"], GREEN, GREEN_PALE),
        (600, "MCP  /mcp", ["SEP-2640 skills/list, skills/get", "skill:// URIs, per-file digests", "+ tools for today's clients"], AMBER, AMBER_PALE),
    ]
    for y, title, lines, accent, pale in surfaces:
        r = (1370, y, 1940, y + 160)
        box(d, r, radius=14, fill=pale, outline=accent, width=2)
        ta(d, title, 1396, y + 24, fonts["h"], accent)
        for i, line in enumerate(lines):
            ta(d, line, 1396, y + 72 + i * 28, fonts["small"], DIM)
        arrow(d, 1290, 470, 1362, y + 80, accent, width=2, head=11)

    # Agents
    box(d, (1370, 790, 1940, 986), radius=14, fill=PAPER, outline=FG, width=2)
    tc(d, "every agent reads SKILL.md natively", 1655, 818, fonts["label"], FG)
    agents = [("Claude Code", ".claude/skills/"), ("Codex", ".agents/skills/"),
              ("Cursor", ".cursor/skills/"), ("Copilot", ".github/skills/"),
              ("Gemini CLI, Kiro, Windsurf", "their own skills folders")]
    for i, (name, path) in enumerate(agents):
        y = 852 + i * 27
        ta(d, name, 1396, y, fonts["small"], FG)
        ta(d, path, 1914, y, fonts["mono_s"], DIM, anchor="rt")
    tc(d, "in the project, or in the home directory that serves every repo",
       1655, 1004, fonts["small"], DIM)

    # ---- The serverless path ---------------------------------------------
    y0 = 1040
    dashed(d, (60, y0, 1940, y0 + 112), SOFT, width=2)
    ta(d, "WITHOUT A SERVER", 92, y0 + 20, fonts["band"], DIM)
    ta(d, "the same command, the same folders, the same digests", 92, y0 + 52, fonts["body"], FG)
    ta(d, "the gateway only adds who may have what", 92, y0 + 80, fonts["small"], DIM)
    ta(d, "sgw sync -out ~", 1914, y0 + 46, fonts["mono"], GREEN, anchor="rt")
    ta(d, "a folder or a git repo  →  every agent, every repo", 1914, y0 + 78, fonts["small"], DIM, anchor="rt")
    arrow(d, 700, y0 + 62, 1520, y0 + 62, GREEN, width=2, head=12, dash=True)

    # ---- Promises strip ---------------------------------------------------
    promises = [
        ("denied looks like missing", "a 404, so policy cannot be used to enumerate", RED, RED_PALE),
        ("published is frozen", "re-publishing a version is refused", AMBER, AMBER_PALE),
        ("verified end to end", "the client recomputes every digest", GREEN, GREEN_PALE),
        ("the agent type is a claim", "read from the token, never a header", TEAL, TEAL_PALE),
    ]
    for i, (title, note, accent, pale) in enumerate(promises):
        x = 60 + i * 478
        r = (x, 1172, x + 448, 1244)
        box(d, r, radius=12, fill=pale, outline=accent, width=2)
        ta(d, title, x + 20, r[1] + 16, fonts["label"], accent)
        ta(d, note, x + 20, r[1] + 44, fonts["small"], DIM)

    img.save(out_path, optimize=True)
    print(f"wrote {out_path} ({out_path.stat().st_size / 1024:.0f} KB, {WIDTH}x{HEIGHT})")
    return out_path


if __name__ == "__main__":
    render()
