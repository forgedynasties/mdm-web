#!/usr/bin/env bash
# Stitch title cards + recorded scenes into out/$OUT (1080p H.264, 30 fps).
#   ./assemble.sh                 # silent
#   MUSIC=track.mp3 ./assemble.sh # with a soundtrack, faded out at the end
#   OUT=teaser.mp4 SCENES="01 02 05 06" SPEED=1.6 CARD_SEC=1.8 ./assemble.sh   # fast subset cut
#   SCENES_DIR=out/scenes_light OUT=reel_light.mp4 ./assemble.sh
# Card clips are CARD_SEC long; scene clips get a short fade in/out.
set -euo pipefail
cd "$(dirname "$0")"
CARD_SEC="${CARD_SEC:-2.6}"
OUT="${OUT:-reel.mp4}"
SCENES_DIR="${SCENES_DIR:-out/scenes}"
SCENES="${SCENES:-}"   # space-separated scene indexes to include (default all)
FADE="${FADE:-0.35}"
SPEED="${SPEED:-1}"      # >1 speeds scene clips up (teaser cuts), cards unaffected
W=1920; H=1080; FPS=30
calc() { awk "BEGIN{printf \"%.3f\", $1}"; }
CLIPS="out/clips_${OUT%.mp4}"; CONCAT="out/concat_${OUT%.mp4}.txt"
mkdir -p "$CLIPS"
rm -f "$CLIPS"/*.mp4 "$CONCAT"

enc=(-c:v libx264 -preset medium -crf 20 -pix_fmt yuv420p -r $FPS -an -movflags +faststart)
vf_scale="scale=$W:$H:force_original_aspect_ratio=decrease,pad=$W:$H:(ow-iw)/2:(oh-ih)/2,setsar=1"

card() { # name png
  ffmpeg -v error -y -loop 1 -t "$CARD_SEC" -i "$2" \
    -vf "$vf_scale,fade=t=in:st=0:d=$FADE,fade=t=out:st=$(calc "$CARD_SEC-$FADE"):d=$FADE,format=yuv420p" \
    "${enc[@]}" "$CLIPS/$1.mp4"
}
scene() { # name webm
  local trim=0; [ -f "${2%.webm}.json" ] && trim=$(sed -E 's/.*"trimMs":([0-9]+).*/\1/' "${2%.webm}.json")
  local ss; ss=$(calc "$trim/1000")
  local dur; dur=$(ffprobe -v error -show_entries format=duration -of csv=p=0 "$2")
  # Playwright's webm sometimes reports no duration; fall back to counting frames.
  if [ -z "$dur" ] || [ "$dur" = "N/A" ]; then dur=$(ffprobe -v error -count_frames -select_streams v:0 -show_entries stream=nb_read_frames -of csv=p=0 "$2"); dur=$(calc "$dur/25"); fi
  dur=$(calc "($dur-$ss)/$SPEED")
  ffmpeg -v error -y -ss "$ss" -i "$2" \
    -vf "setpts=PTS/$SPEED,$vf_scale,fade=t=in:st=0:d=$FADE,fade=t=out:st=$(calc "$dur-$FADE"):d=$FADE,format=yuv420p" \
    "${enc[@]}" "$CLIPS/$1.mp4"
}

echo "cards…";  card intro out/cards/00-intro.png
for s in "$SCENES_DIR"/*.webm; do
  n=$(basename "$s" .webm)            # 01-overview
  idx=${n%%-*}; name=${n#*-}
  if [ -n "$SCENES" ] && ! grep -qw "$idx" <<<"$SCENES"; then continue; fi
  c="out/cards/${idx}-${name}.png"
  [ -f "$c" ] && card "${idx}a-${name}-card" "$c"
  echo "scene $n…"; scene "${idx}b-${name}" "$s"
done
card outro out/cards/99-outro.png

for f in $CLIPS/intro.mp4 $(ls $CLIPS/[0-9]*.mp4 | sort) $CLIPS/outro.mp4; do echo "file '$(pwd)/$f'" >> $CONCAT; done

if [ -n "${MUSIC:-}" ]; then
  ffmpeg -v error -y -f concat -safe 0 -i $CONCAT -i "$MUSIC" \
    -filter_complex "[1:a]afade=t=in:st=0:d=1[a]" -map 0:v -map "[a]" -c:v copy -c:a aac -b:a 160k -shortest \
    -af "afade=t=out:st=$(calc "$(ffprobe -v error -f concat -safe 0 -show_entries format=duration -of csv=p=0 $CONCAT) - 2"):d=2" \
    out/$OUT 2>/dev/null || ffmpeg -v error -y -f concat -safe 0 -i $CONCAT -i "$MUSIC" -map 0:v -map 1:a -c:v copy -c:a aac -b:a 160k -shortest out/$OUT
else
  ffmpeg -v error -y -f concat -safe 0 -i $CONCAT -c copy out/$OUT
fi
echo "→ out/$OUT  ($(ffprobe -v error -show_entries format=duration -of csv=p=0 out/$OUT | cut -c1-5)s)"
