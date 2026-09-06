#!/usr/bin/env bash
# Start/stop the fake-device simulator in the background (pidfile in out/).
#   ./sim.sh start | stop | status
set -e
cd "$(dirname "$0")"
mkdir -p out
case "${1:-status}" in
  start)
    if [ -f out/sim.pid ] && kill -0 "$(cat out/sim.pid)" 2>/dev/null; then echo "already running (pid $(cat out/sim.pid))"; exit 0; fi
    nohup node fleet-sim.mjs > out/sim.log 2>&1 &
    echo $! > out/sim.pid
    sleep 6; tail -c 200 out/sim.log; echo ;;
  stop)
    if [ -f out/sim.pid ]; then kill "$(cat out/sim.pid)" 2>/dev/null && echo stopped; rm -f out/sim.pid; else echo "not running"; fi ;;
  status)
    if [ -f out/sim.pid ] && kill -0 "$(cat out/sim.pid)" 2>/dev/null; then echo "running (pid $(cat out/sim.pid))"; tail -c 100 out/sim.log; echo; else echo "not running"; fi ;;
esac
