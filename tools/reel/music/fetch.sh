#!/usr/bin/env bash
# Download the reel's soundtrack candidates from incompetech.com.
# Kevin MacLeod, licensed CC BY 4.0 — keep the credit on the outro card.
set -e
cd "$(dirname "$0")"
for t in "Inspired" "Deliberate Thought" "Dispersion Relation" "Wallpaper" "Digital Lemonade"; do
  [ -f "$t.mp3" ] && { echo "have $t"; continue; }
  url="https://incompetech.com/music/royalty-free/mp3-royaltyfree/$(python3 -c 'import urllib.parse,sys;print(urllib.parse.quote(sys.argv[1]))' "$t").mp3"
  curl -sL -A "Mozilla/5.0" "$url" -o "$t.mp3" && echo "got $t"
done
