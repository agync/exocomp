# Trimming: the tight action-only step cut

A raw demo has the waits between actions. The reliable way to drop them is a
**step cut**: keep a short window around each step you actually captured (a
screenshot marks it), select-concat those windows, then speed up. This is the
tool-native approach — no frame-diff guessing.

## Why not frame-diff (freezedetect/scene)

Both `freezedetect` and scene detection fail on a live UI demo:
- **freezedetect** can't separate a genuine wait (idle page) from **log
  streaming** (the log lines change, but too subtly for the threshold) — at a
  coarse threshold the whole run looks "frozen"; at a fine one you catch a
  ~0.04s heartbeat blip every 5s (the clock tick).
- **scene detection** (`select='gt(scene,0.3)'`) only fires on hard cuts; web
  transitions are smooth and most actions (scroll, dialog open) are on the same
  page, so it catches almost nothing.

The screenshots you take per step ARE the action anchors. Use them.

## The recipe

1. Take a screenshot at each demo step (see `screenshots.md`). Record the
   wall-clock capture time (the file mtime is fine).

2. Map screenshot wall-times to video times. Anchor on a known-good moment:
   pick one screenshot whose video time you can pin — e.g. a step whose page is
   unmistakable in the video at a sampled frame — and solve the video start from
   it:
   ```python
   import datetime as dt
   anchor_wall = dt.datetime(2026,9,10,21,28,48)   # a screenshot's mtime
   anchor_vid  = 750.0                              # its video time (sample frames to find)
   video_start = anchor_wall - dt.timedelta(seconds=anchor_vid)
   ```

3. Build keep-windows: each step = `[vid_time - 1.5, vid_time + 6]` (a bit
   before to catch the action, a bit after to settle). Add the login segment at
   the start.

4. Select-concat with ffmpeg:
   ```sh
   # windows.json = [[s,e], ...] in seconds
   python3 -c '
   import json
   wins = json.load(open("windows.json"))
   print("select=" + "+".join("between(t,%.3f,%.3f)" % (s,e) for s,e in wins))
   ' > /tmp/cutfilt.txt
   ffmpeg -i <name>.webm -vf "$(cat /tmp/cutfilt.txt),setpts=N/FRAME_RATE/TB" \
     -c:v libvpx -b:v 6000k <name>-steps.webm
   ```

5. Speed up (optional; 2x via `setpts=0.5*PTS`, 3x via `0.33*PTS`):
   ```sh
   ffmpeg -i <name>-steps.webm -filter:v "setpts=0.5*PTS" -r 25 \
     -c:v libvpx -b:v 6000k -crf 28 -an <name>-steps-2x.webm
   ```

## The gotcha: the video can drift from the screenshots

The recorder's capture can diverge from what the live page shows — seen in the
v7 demo: a mid-recording **light-theme excursion** (the video ran light for ~60s
while the screenshots stayed dark), and reload white-flashes. So:

- **Screenshots are ground truth** for theme and step content.
- If a step's video window doesn't match its screenshot (check brightness, see
  `verification.md`), **re-anchor** that step: scan the surrounding video for a
  frame matching the screenshot (dark = mean ~16-19, light = ~239), and move the
  window there.
- After the cut, verify the whole arc is coherent (light login → dark body →
  dark end).

## Result

The v7 k3s demo: raw 1072s → step cut 123s → 2x = 62s, 3.5MB, all 12 steps
(login, dark switch, workspaces columns, backend block, log tail, plans tray,
plan diff, apply dialog, apply succeeded, drift plan, drift strike, restore).
