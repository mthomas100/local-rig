#!/usr/bin/env python3
"""Replay a captured terminal session as an animated SVG (no dependencies).

    docs/charts/make_terminal_svg.py docs/data/live-held-session-2026-10-05.txt docs/media/hold-live-session.svg

The input is the session as captured (the HTTP Date header was dropped: it gives away the timezone): lines starting
with "$ " are commands, everything else is their output. Long lines soft-wrap at COLS like a terminal; nothing is
reworded. In a browser, commands type out and output appears after them, so a capture taken at t=0 is an empty
terminal. The finished transcript is the static default: every animation sits inside
`@media (prefers-reduced-motion: no-preference)` and only hides text until its moment, so renderers that ignore
CSS animation (rsvg-convert, many thumbnailers) and viewers who ask for reduced motion see the final frame.
"""
import html
import sys

COLS, CH, LH, PAD, TOP, GAP = 104, 8.4, 19, 18, 30, 9
TYPE_CPS, PAUSE = 55, 0.7  # typing speed (characters per second) and the pause after each command's output
BG, BAR, FG, DIM, PROMPT, CMD, WARN = "#14161a", "#22252b", "#d7dae0", "#8b919c", "#5ec4a0", "#ffffff", "#f0a35e"


def wrap(line):
    return [line[i:i + COLS] for i in range(0, max(len(line), 1), COLS)] or [""]


def color(text, is_cmd):
    if is_cmd:
        return CMD
    if text.startswith(("HTTP/1.1 503", "Retry-After")) or "refused" in text or text.startswith("held:"):
        return WARN
    return FG


src, out = sys.argv[1], sys.argv[2]
rows, t, css = [], 0.6, []  # rows: (text, colour, start time, typing duration or 0)
for raw in open(src).read().splitlines():
    if raw.startswith("$ "):
        t += PAUSE
        for k, part in enumerate(wrap(raw)):
            dur = len(part) / TYPE_CPS
            rows.append((part, CMD, t, dur, k == 0))
            t += dur
        t += 0.25
    else:
        for part in wrap(raw):
            rows.append((part, color(raw, False), t, 0, False))
            t += 0.06

gaps = sum(1 for i, r in enumerate(rows) if r[4] and i)
height = TOP + PAD + len(rows) * LH + gaps * GAP
width = PAD * 2 + COLS * CH
body = []
y = TOP + PAD - LH + 13
for i, (text, col, start, dur, prompt) in enumerate(rows):
    y += LH + (GAP if prompt and i else 0)
    esc = html.escape(text, quote=False).replace(" ", "\u00a0")
    if dur:
        w = len(text) * CH
        css.append(f".c{i}{{animation:show 0s {start:.2f}s backwards, type{i} {dur:.2f}s steps({max(len(text), 1)}) {start:.2f}s backwards}}"
                   f"@keyframes type{i}{{from{{clip-path:inset(0 {w:.0f}px 0 0)}}to{{clip-path:inset(0 0 0 0)}}}}")
        body.append(f'<text class="c{i}" x="{PAD}" y="{y}" fill="{col}">{esc}</text>')
        if prompt:  # colour the "$" like a prompt
            body.append(f'<text class="c{i}" x="{PAD}" y="{y}" fill="{PROMPT}">$</text>')
    else:
        css.append(f".c{i}{{animation:show 0s {start:.2f}s backwards}}")
        body.append(f'<text class="c{i}" x="{PAD}" y="{y}" fill="{col}">{esc}</text>')

svg = f"""<svg xmlns="http://www.w3.org/2000/svg" xml:space="preserve" width="{width:.0f}" height="{height:.0f}" viewBox="0 0 {width:.0f} {height:.0f}" role="img" aria-label="Terminal session: hold status, a request refused with 503 while the GPU is held, nothing loaded">
<style>
text{{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:13.5px;white-space:pre}}
@keyframes show{{from{{opacity:0}}to{{opacity:0}}}}
@media (prefers-reduced-motion:no-preference){{
{chr(10).join(css)}
}}
</style>
<rect width="100%" height="100%" rx="10" fill="{BG}"/>
<rect width="100%" height="30" rx="10" fill="{BAR}"/><rect y="20" width="100%" height="10" fill="{BAR}"/>
<circle cx="18" cy="15" r="5.5" fill="#ff5f57"/><circle cx="36" cy="15" r="5.5" fill="#febc2e"/><circle cx="54" cy="15" r="5.5" fill="#28c840"/>
<text x="{width / 2:.0f}" y="19" fill="{DIM}" text-anchor="middle" style="font-size:12px">local-rig: the gate while a render holds the GPU (real session, 2026-10-05 21:42)</text>
{chr(10).join(body)}
</svg>
"""
open(out, "w").write(svg)
print(f"wrote {out}: {len(rows)} rows, {t:.1f} s of animation")
