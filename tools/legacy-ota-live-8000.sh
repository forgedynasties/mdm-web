#!/usr/bin/env bash
# Move the legacy OTA port 8000 from the stage instance to the live MDM.
# Run on the dev box as root. Steps:
#   1. stage: LEGACY_OTA_PORT=8010, recreate (frees 8000)
#   2. live: keep the box-local docker-compose.yml edit (stash/pop), pull live-dpc,
#      LEGACY_OTA_PORT=8000 in .env, rebuild, recreate
#   3. verify: 8000 answers from live, stage on 8010, old ota-server on 8001
# Revert: swap the two LEGACY_OTA_PORT values back and `docker compose up -d server` in both.
set -e
echo "== stage -> 8010"
cd /home/ubuntu/stage-mdm
grep -q '^LEGACY_OTA_PORT=' .env && sed -i 's/^LEGACY_OTA_PORT=.*/LEGACY_OTA_PORT=8010/' .env || echo 'LEGACY_OTA_PORT=8010' >> .env
docker compose up -d server 2>&1 | tail -1
sleep 5
echo "== live: pull live-dpc, keep local compose edit"
cd /home/ubuntu/mdm
G="git -c safe.directory=/home/ubuntu/mdm"
echo "before: $($G log --oneline -1)"
$G stash push -m "box-local compose edit (kept by deploy)" -- docker-compose.yml >/dev/null || true
KEY=$(ls ./github_aio 2>/dev/null || find /home/ubuntu -maxdepth 3 -name github_aio 2>/dev/null | head -1)
GIT_SSH_COMMAND="ssh -i $KEY -o StrictHostKeyChecking=accept-new" $G pull --ff-only origin live-dpc 2>&1 | tail -2
$G stash pop >/dev/null 2>&1 && echo "local compose edit restored" || echo "no local edit to restore (or conflict: see git stash list)"
echo "after: $($G log --oneline -1)"
grep -q '^LEGACY_OTA_PORT=' .env && sed -i 's/^LEGACY_OTA_PORT=.*/LEGACY_OTA_PORT=8000/' .env || echo 'LEGACY_OTA_PORT=8000' >> .env
docker compose build server 2>&1 | tail -2
docker compose up -d server 2>&1 | tail -1
sleep 15
echo "== verify"
docker ps --format '{{.Names}} {{.Status}} {{.Ports}}' | grep -E 'mdm-server-1|stage-mdm-server-1|ota-server'
docker logs --since 1m mdm-server-1 2>&1 | grep -iE 'legacy|listen|panic' | tail -3
echo -n "live 8000 healthz: "; curl -s http://127.0.0.1:8000/healthz; echo
echo -n "stage 8010 healthz: "; curl -s http://127.0.0.1:8010/healthz; echo
curl -s -o /dev/null -w "old ota-server 8001: %{http_code}\n" http://127.0.0.1:8001/docs
curl -s -o /dev/null -w "live login page: %{http_code}\n" https://mdm.dev.aioapp.com/login
