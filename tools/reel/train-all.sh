#!/usr/bin/env bash
# Narrate, record and render a list of chapters, one after another.
#   ./train-all.sh ch02 ch03 …        (default: every chapters/*.mjs)
set -u
cd "$(dirname "$0")"
CH=("$@"); [ ${#CH[@]} -eq 0 ] && CH=($(ls chapters/*.mjs | xargs -n1 basename | sed 's/\.mjs$//'))
for c in "${CH[@]}"; do
  echo "== $c"
  node narrate.mjs "$c" 2>&1 | tail -1
  node training.mjs "$c" 2>&1 | grep -E "!|→"
  rm -rf "remotion/public/$c"; mkdir -p "remotion/public/$c"
  cp -r "out/training/$c/take.webm" "out/training/$c/take.json" "out/training/$c/narration.json" "out/training/$c/audio" "remotion/public/$c/"
  ( cd remotion && docker run --rm -v "$PWD":/app -w /app -u "$(id -u):$(id -g)" -e HOME=/tmp reel-remotion \
      sh -c "npx remotion render src/index.ts Chapter out/$c.mp4 --props='{\"chapter\":\"$c\"}' --log=error" 2>&1 | tail -2 )
  cp "remotion/out/$c.mp4" "out/training_$c.mp4" 2>/dev/null && echo "→ out/training_$c.mp4"
done
echo ALL_DONE
