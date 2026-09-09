#!/usr/bin/env bash
# Legacy OTA cutover on the dev box (run there, e.g. via SSM as ubuntu/root):
#   1. move the old ota-server container from host port 8000 to 8001
#   2. pull + rebuild the MDM (live-dpc), whose compose now publishes 8000
#   3. verify both listeners
#
# Revert (old server back on 8000):
#   cd /home/ubuntu/mdm && docker compose stop server
#   cd /home/ubuntu/ota_server && cp docker-compose.yml.bak-port8000 docker-compose.yml && docker compose up -d
#   (then start the MDM again with LEGACY_OTA_PORT unset in .env, or leave it stopped)
# Soft revert without touching containers: Settings › Legacy OTA server › "Pass through".
set -e

echo "== move ota-server to host port 8001"
cd /home/ubuntu/ota_server
cp -n docker-compose.yml docker-compose.yml.bak-port8000 || true
sed -i 's/"8000:8000"/"8001:8000"/' docker-compose.yml
grep -n '8001:8000' docker-compose.yml
docker compose up -d 2>&1 | tail -1
sleep 3
docker ps --format '{{.Names}} {{.Ports}}' | grep ota-server
curl -s -o /dev/null -w "old ota-server on 8001: %{http_code}\n" http://127.0.0.1:8001/docs

echo "== deploy MDM (live-dpc)"
cd /home/ubuntu/mdm
echo "before: $(git -c safe.directory=/home/ubuntu/mdm log --oneline -1)"
KEY=$(ls ./github_aio 2>/dev/null || find /home/ubuntu -maxdepth 3 -name github_aio 2>/dev/null | head -1)
GIT_SSH_COMMAND="ssh -i $KEY -o StrictHostKeyChecking=accept-new" git -c safe.directory=/home/ubuntu/mdm pull --ff-only origin live-dpc 2>&1 | tail -2
echo "after: $(git -c safe.directory=/home/ubuntu/mdm log --oneline -1)"
docker compose build server 2>&1 | tail -2
docker compose up -d server 2>&1 | tail -1
sleep 15
docker ps --format '{{.Names}} {{.Status}} {{.Ports}}' | grep -E '^mdm-server-1'
docker logs --since 1m mdm-server-1 2>&1 | grep -iE 'listen|legacy|panic|error' | tail -4

echo "== verify"
curl -s -o /dev/null -w "public login page %{http_code}\n" https://mdm.dev.aioapp.com/login
curl -s http://127.0.0.1:8000/healthz; echo
curl -s -o /dev/null -w "public 8000 healthz: %{http_code}\n" http://18.237.229.116:8000/healthz
curl -s -o /dev/null -w "8000 dashboard blocked (want 404): %{http_code}\n" http://127.0.0.1:8000/devices
