#!/usr/bin/env bash
# Put the STAGE MDM's legacy OTA listener on the port the devices have baked in.
# Runs on the dev box (SSM shell as root). Touches only:
#   - the old ota-server container: host port 8000 -> 8001 (dashboard via tunnel)
#   - the stage instance (/home/ubuntu/stage-mdm): LEGACY_OTA_PORT=8000 in .env
# The live MDM (/home/ubuntu/mdm) is not touched.
#
# Revert: cd /home/ubuntu/stage-mdm && sed -i 's/^LEGACY_OTA_PORT=.*/LEGACY_OTA_PORT=8010/' .env && docker compose up -d server
#         cd /home/ubuntu/ota_server && cp docker-compose.yml.bak-port8000 docker-compose.yml && docker compose up -d
# Soft revert (keeps ports): stage Settings › Legacy OTA server › "Pass through".
set -e
echo "== old ota-server: 8000 -> 8001"
cd /home/ubuntu/ota_server
cp -n docker-compose.yml docker-compose.yml.bak-port8000 || true
sed -i 's/"8000:8000"/"8001:8000"/' docker-compose.yml
grep -n '8001:8000' docker-compose.yml
docker compose up -d 2>&1 | tail -1
sleep 3
curl -s -o /dev/null -w "old ota-server on 8001: %{http_code}\n" http://127.0.0.1:8001/docs

echo "== stage: legacy listener on 8000"
cd /home/ubuntu/stage-mdm
grep -q '^LEGACY_OTA_PORT=' .env && sed -i 's/^LEGACY_OTA_PORT=.*/LEGACY_OTA_PORT=8000/' .env || echo 'LEGACY_OTA_PORT=8000' >> .env
grep '^LEGACY_OTA_PORT' .env
docker compose up -d server 2>&1 | tail -1
sleep 12
docker ps --format '{{.Names}} {{.Status}} {{.Ports}}' | grep -E 'stage-mdm-server|ota-server'
docker logs --since 1m stage-mdm-server-1 2>&1 | grep -iE 'legacy|listen' | tail -2
echo -n "stage healthz on 8000: "; curl -s http://127.0.0.1:8000/healthz; echo
curl -s -o /dev/null -w "public 8000 healthz: %{http_code}\n" http://18.237.229.116:8000/healthz
curl -s -o /dev/null -w "stage login page %{http_code}\n" https://mdm-stage.dev.aioapp.com/login
