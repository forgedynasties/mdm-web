#!/usr/bin/env bash
#
# One-command launcher for the stage MDM deploy.
#
# Reproduces the manual steps in a single command:
#   aws ssm start-session --target i-0bda11fb1feceaa74
#   sudo su
#   cd /home/ubuntu/stage-mdm
#   ./ci
#
# It opens an SSM session, becomes root, cds into the deploy dir, and runs the CD
# watcher (./ci) — you stay attached to its output; Ctrl-C detaches/stops it.
#
# Usage:
#   ./stage-deploy.sh            # run ./ci (the continuous-deploy watcher)
#   ./stage-deploy.sh bash       # just drop into a root shell in the deploy dir
#   ./stage-deploy.sh 'git pull' # run any command as root in the deploy dir
#
# Override target/dir with env vars if they ever change:
#   STAGE_INSTANCE=i-xxxx STAGE_DIR=/home/ubuntu/stage-mdm ./stage-deploy.sh
#
# Requires the AWS CLI + the Session Manager plugin, with credentials that can reach
# the instance (the same setup your manual `aws ssm start-session` already uses).
set -euo pipefail

INSTANCE="${STAGE_INSTANCE:-i-0bda11fb1feceaa74}"
DIR="${STAGE_DIR:-/home/ubuntu/stage-mdm}"
RUN="${1:-./ci}"
# A bare shell needs -i to be interactive under the PTY; commands run as given.
[ "$RUN" = "bash" ] && RUN="bash -i"
[ "$RUN" = "sh" ]   && RUN="sh -i"

command -v aws >/dev/null 2>&1 || { echo "aws CLI not found on PATH" >&2; exit 1; }

# The command run under the session's PTY: become root, cd into the deploy dir, exec RUN.
CMD="sudo bash -lc 'cd $DIR && exec $RUN'"

# --parameters needs JSON (the shorthand key=value form can't cope with spaces/quotes).
# `command` is a StringList, so: {"command":["<CMD>"]}. Build it with a real JSON encoder
# so any quoting in CMD is escaped correctly.
if command -v jq >/dev/null 2>&1; then
  PARAMS=$(jq -nc --arg c "$CMD" '{command:[$c]}')
elif command -v python3 >/dev/null 2>&1; then
  PARAMS=$(python3 -c 'import json,sys; print(json.dumps({"command":[sys.argv[1]]}))' "$CMD")
else
  PARAMS="{\"command\":[\"$CMD\"]}"   # best-effort; assumes DIR/RUN have no double quotes
fi

exec aws ssm start-session \
  --target "$INSTANCE" \
  --document-name AWS-StartInteractiveCommand \
  --parameters "$PARAMS"
