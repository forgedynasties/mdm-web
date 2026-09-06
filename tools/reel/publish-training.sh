#!/usr/bin/env bash
# Publish rendered training chapters to S3 for the landing page.
#   ./publish-training.sh            # every out/training_chNN.mp4
# Uploads training/chNN.mp4, a poster training/chNN.jpg (frame at 4 s) and
# training/manifest.json (id, number, title, intro, steps, duration) to the bucket the
# server already uses (S3_BUCKET in ../../.env). The landing page presigns them.
set -euo pipefail
cd "$(dirname "$0")"
BUCKET=$(grep -E '^S3_BUCKET=' ../../.env | cut -d= -f2)
REGION=$(grep -E '^AWS_REGION=' ../../.env | cut -d= -f2)
FF=$(node -e "console.log(require('ffmpeg-static'))")
mkdir -p out/publish
echo '{"chapters":[' > out/publish/manifest.json
first=1
for mp4 in $(ls out/training_ch*.mp4 | sort); do
  id=$(basename "$mp4" .mp4); id=${id#training_}
  take="out/training/$id/take.json"; [ -f "$take" ] || { echo "skip $id (no take.json)"; continue; }
  dur=$(ffprobe -v error -show_entries format=duration -of csv=p=0 "$mp4" | cut -d. -f1)
  "$FF" -v error -y -ss 14 -i "$mp4" -frames:v 1 -vf scale=960:-1 -q:v 4 "out/publish/$id.jpg"
  node -e "const t=require('./$take');const o={id:'$id',number:t.number,title:t.title,intro:t.intro,steps:t.steps.length,duration:$dur};process.stdout.write(($first?'':',')+JSON.stringify(o))" >> out/publish/manifest.json
  first=0
  aws s3 cp "$mp4" "s3://$BUCKET/training/$id.mp4" --region "$REGION" --content-type video/mp4 --only-show-errors
  aws s3 cp "out/publish/$id.jpg" "s3://$BUCKET/training/$id.jpg" --region "$REGION" --content-type image/jpeg --only-show-errors
  echo "↑ $id (${dur}s)"
done
echo ']}' >> out/publish/manifest.json
aws s3 cp out/publish/manifest.json "s3://$BUCKET/training/manifest.json" --region "$REGION" --content-type application/json --only-show-errors
echo "↑ manifest: $(node -e "console.log(require('./out/publish/manifest.json').chapters.map(c=>c.id).join(' '))")"
