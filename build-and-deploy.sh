#!/bin/sh
# build the latest release from this repo and drop it into ../agent-router-deploy,
# then restart the deploy instance. usage: ./build-and-deploy.sh
set -e
cd "$(dirname "$0")"

DEPLOY="${AGENT_ROUTER_DEPLOY:-$(pwd)/../agent-router-deploy}"

echo "==> building dashboard (web/)"
(cd web && npm run build --silent)

echo "==> building agent-router"
go build -o "$DEPLOY/bin/agent-router" ./cmd/agent-router

echo "==> installed: $DEPLOY/bin/agent-router"
"$DEPLOY/bin/agent-router" version

echo "==> (re)starting deploy instance"
sh "$DEPLOY/run.sh"
