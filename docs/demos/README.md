# Demos

`skills-gateway-explainer.gif` is the square explainer used at the top of the
project README. It walks through eight beats in about twenty-two seconds: a skill
folder copied by hand to every agent, why that matters, the gateway in the
middle, one request decided by identity and policy, how a version is
fingerprinted, and the three ways the same governed catalog is served.

## Re-rendering

The renderer is deterministic — no randomness, no network, no input files — so
re-running it overwrites the GIF with an identical result unless the script
changed.

```bash
python docs/demos/render_explainer.py
```

Output: 1080x1080, 12 fps, ~260 frames, palette-quantised to 64 colours.

## Pacing

`SPEED` at the top of the script stretches every hold and every stagger;
`BEAT_FRAMES_BASE` is how long each beat runs at `SPEED = 1.0`, where 12
frames is one second. Raise `SPEED` until a first-time reader can finish the
densest beat without pausing the GIF. Held frames are pixel-identical and
merge on export, so a slower GIF costs very little extra size.

## Requirements

- `pillow` (`pip install pillow`)
- Fonts: Segoe UI, Arial, or DejaVu Sans, plus a monospace face (Cascadia Mono,
  Consolas or DejaVu Sans Mono). The script falls back through that list and
  never fails on a missing font.

## Editing

Each beat is one function (`beat1` … `beat8`) that draws a full frame, given
how many frames into the beat it is. `BEAT_FRAMES_BASE` sets how long each beat runs
before `SPEED` is applied. `pop()` returns a 0..1 ease for staggering
elements into view, and scales with `SPEED` too.

Any claim on screen must match the repository: the 36.8% figure is Snyk's
ToxicSkills audit (February 2026, 3,984 skills from ClawHub and skills.sh), and
the behaviour shown in beats 5 and 6 is covered by tests in `internal/registry`
and `internal/bundle`.
