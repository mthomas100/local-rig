#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.11"
# dependencies = ["matplotlib>=3.8"]
# ///
"""Draw the README charts from docs/data/*.csv (no model, no GPU).

    docs/charts/make_charts.py          # writes docs/media/hold-live-checks.png and hold-first-day.png

The CSVs are the hold gate's own log (~/.ds4/hold-gate.log) from 2026-10-04 21:30 to 2026-10-05 21:18, reduced to
one row per hold, refusal and unload, with job names shortened and per-machine details removed.
"""
import csv
import pathlib
from datetime import datetime, timedelta

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt
from matplotlib.lines import Line2D
from matplotlib.patches import Patch

ROOT = pathlib.Path(__file__).resolve().parents[1]
DATA, MEDIA = ROOT / "data", ROOT / "media"

SURFACE, INK, INK2, GRID = "#fcfcfb", "#0b0b0b", "#52514e", "#e4e3df"
HELD, WAIT, ANSWER = "#2a78d6", "#eb6834", "#1baf7a"  # categorical slots 1-3, validated (light surface)

plt.rcParams.update({
    "font.family": "sans-serif", "font.size": 11, "axes.edgecolor": GRID, "axes.labelcolor": INK2,
    "xtick.color": INK2, "ytick.color": INK, "axes.facecolor": SURFACE, "figure.facecolor": SURFACE,
    "savefig.facecolor": SURFACE,
})


def when(s):
    return datetime.fromisoformat(s) if s else None


def rows(name):
    with open(DATA / name) as f:
        return list(csv.DictReader(f))


holds = [dict(r, requested=when(r["requested"]), granted=when(r["granted"]), released=when(r["released"]))
         for r in rows("hold-gate-holds.csv")]
refusals = [dict(r, time=when(r["time"])) for r in rows("hold-gate-refusals.csv")]


def style(ax):
    for s in ("top", "right", "left"):
        ax.spines[s].set_visible(False)
    ax.tick_params(axis="y", length=0)
    ax.grid(axis="x", color=GRID, linewidth=0.8)
    ax.set_axisbelow(True)


# ---------- 1. the live checks, 2026-10-04 21:30-21:42 ----------
live = holds[:4]
t0 = datetime(2026, 10, 4, 21, 30)
sec = lambda t: (t - t0).total_seconds()
# The two pi answers are not in the gate's log (pi does not report to it); docs/hold.md records them: a prompt sent
# during the first manual hold answered 9 s after `hold off`, one sent mid-render answered 17 s after the release.
answers = {0: 9, 2: 17}
names = ["hold on: curl probes", "hold on: Codex", "film render (2 LTX clips)", "m3d concept stage"]

fig, ax = plt.subplots(figsize=(12, 4.8), dpi=130)
style(ax)
for i, h in enumerate(live):
    y = len(live) - 1 - i
    ax.barh(y, sec(h["granted"]) - sec(h["requested"]), left=sec(h["requested"]), height=0.5, color=WAIT)
    ax.barh(y, sec(h["released"]) - sec(h["granted"]), left=sec(h["granted"]), height=0.5, color=HELD)
    held = (h["released"] - h["granted"]).total_seconds()
    mins = f"{int(held // 60)}m{int(held % 60):02d}s" if held >= 60 else f"{int(held)} s"
    ax.text(sec(h["released"]) + 6, y + 0.3, f"held {mins}", va="center", color=INK2, fontsize=10)
    for r in refusals:
        if h["requested"] <= r["time"] <= h["released"]:
            ax.plot(sec(r["time"]), y, marker="x", color=INK, markersize=8, markeredgewidth=2)
    if i in answers:
        t = sec(h["released"]) + answers[i]
        ax.plot(t, y, marker="o", color=ANSWER, markersize=9, markeredgecolor=SURFACE, markeredgewidth=2)
        ax.text(t + 6, y - 0.22, f"pi answers +{answers[i]} s", va="center", color=INK, fontsize=10)
ax.set_yticks(range(len(live)), names[::-1])
ticks = range(0, 13 * 60, 60)
ax.set_xticks(list(ticks), [f"21:{30 + m // 60:02d}" for m in ticks])
ax.set_xlim(40, 12 * 60 + 30)
ax.set_ylim(-0.6, len(live) - 0.4)
ax.set_xlabel("2026-10-04, local time")
ax.set_title("The hold gate's live checks: nothing reached the model while held", loc="left", color=INK,
             fontsize=13, pad=44)
ax.legend(handles=[Patch(color=WAIT, label="drain + unload (request → grant)"),
                   Patch(color=HELD, label="GPU held: model calls wait or get 503"),
                   Line2D([], [], marker="x", color=INK, ls="", markersize=8, markeredgewidth=2,
                          label="request refused (503, Retry-After 30)"),
                   Line2D([], [], marker="o", color=ANSWER, ls="", markersize=9, label="waiting pi prompt answered")],
          loc="upper left", bbox_to_anchor=(0, 1.2), ncol=2, frameon=False, fontsize=9.5, handlelength=1.2)
fig.text(0.01, 0.01, "Source: the gate's log (docs/data/hold-gate-*.csv) and docs/hold.md; Apple M5 Max 128 GB, "
         "model qwen38. Codex retried every 30 s, honouring Retry-After.", color=INK2, fontsize=9)
fig.tight_layout(rect=(0, 0.04, 1, 1))
fig.savefig(MEDIA / "hold-live-checks.png")
plt.close(fig)

# ---------- 2. the first day of real use ----------
done = [h for h in holds if h["released"]]  # the last hold was still running when the log was read
fig, ax = plt.subplots(figsize=(12, 10), dpi=130)
style(ax)
labels = []
for i, h in enumerate(done):
    y = len(done) - 1 - i
    wait = (h["granted"] - h["requested"]).total_seconds() / 60
    held = (h["released"] - h["granted"]).total_seconds() / 60
    ax.barh(y, wait, height=0.62, color=WAIT)
    ax.barh(y, held, left=wait + (0.15 if wait else 0), height=0.62, color=HELD)
    if wait >= 5:
        ax.text(wait / 2, y, f"waited {wait:.0f} min", va="center", ha="center", color="white", fontsize=9,
                fontweight="bold")
    labels.append(f"{h['requested']:%a %H:%M}  {h['label']}")
ax.set_yticks(range(len(done)), labels[::-1], fontsize=9.5)
ax.set_ylim(-0.6, len(done) - 0.4)
ax.set_xlabel("minutes")
ax.set_title("Every hold in the gate's first day: GPU jobs queued first come, first served", loc="left", color=INK,
             fontsize=13, pad=24)
ax.legend(handles=[Patch(color=WAIT, label="waiting (queue, drain, unload)"),
                   Patch(color=HELD, label="holding the GPU")],
          loc="upper left", bbox_to_anchor=(0, 1.04), ncol=2, frameon=False, fontsize=10, handlelength=1.2)
fig.text(0.01, 0.008, f"Source: docs/data/hold-gate-holds.csv, from the gate's log, 2026-10-04 21:30 to 2026-10-05 21:18 "
         f"({len(done)} finished holds;\none still running is left out). 'outside any hold' rows are GPU jobs the gate found in the "
         "process table and closed the gate for.", color=INK2, fontsize=8.5)
fig.tight_layout(rect=(0, 0.035, 1, 1))
fig.savefig(MEDIA / "hold-first-day.png")
plt.close(fig)
print("wrote", MEDIA / "hold-live-checks.png", "and", MEDIA / "hold-first-day.png")
