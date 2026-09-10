# Verification: I can't see the video — check by numbers

The agent recording the demo cannot view video (no image input). These checks
make that safe: they verify duration, frame count, and the theme arc, then the
human eyeballs the final cut before sharing.

## 1. Duration + frame count

```sh
ffprobe -v error -show_entries format=duration,size -show_entries stream=codec_name,width,height,r_frame_rate \
  -of default=noprint_wrappers=1 <name>.webm
ffprobe -v error -count_frames -select_streams v:0 -show_entries stream=nb_read_frames \
  -of default=noprint_wrappers=1 <name>.webm
```
- Expect `width=1920 height=1080` (the native `--size` capture) and a frame
  count ≈ `duration × 25`.
- A duration far short of the wall-clock session means the recorder elided idle
  frames (the CLI recorder is wall-clock; a raw playwright `recordVideo` is NOT —
  prefer the CLI).

## 2. The theme arc (the load-bearing check)

Sample frame brightness at ~10 points across the video; extract one frame at
each and read its mean luma:

```sh
for pct in 5 20 45 70 95; do
  ffmpeg -v error -ss $((<dur>*pct/100)) -i <name>.webm -frames:v 1 -y /tmp/f.png 2>&1
  identify -format "pct $pct: %[fx:mean*255]\n" /tmp/f.png
done
```

Expected for the exocomp demo:
- **login light** (mean ~238-240), then **dark** (mean ~16-19) after the theme
  switch, **dark to the end**.
- A mean ~**255** (or ~254.955) mid-video is a **light excursion or reload
  flash** — find and re-cut that window (`trimming.md` re-anchoring).

A `mean ~16` with a `std ~12-15` is a real dark UI frame (not a black frame);
a black frame would be ~0.

## 3. Step-cut coherence

After the trim, sample inside each kept window — the frames at the window's
start should already show the step's page (the `-1.5s` pre-roll catches the
action, not blank).

## 4. Content spot-check

For a step whose key fact is text (a diff, a marker value), you can confirm it
appeared in the DOM during recording via the playwright snapshot at that moment
— the screenshots are the record. The video is the moving version of what the
screenshots prove.

## 5. The human gate

Every step above passes by numbers, but the final call is: **eyeball the cut
once before sharing.** The recipe minimizes the risk of a bad frame, it does
not eliminate the need for a human look.
