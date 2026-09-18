#!/bin/sh
# build the latest release from this repo and drop it into ../yardmaster-deploy,
# then restart the deploy instance. usage: ./build-and-deploy.sh
set -e
cd "$(dirname "$0")"

DEPLOY="${AGENT_ROUTER_DEPLOY:-$(pwd)/../yardmaster-deploy}"

echo "==> building dashboard (web/)"
(cd web && npm run build --silent)

echo "==> building yardmaster"
go build -ldflags "-X main.version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)" -o "$DEPLOY/bin/yardmaster" ./cmd/yardmaster

echo "==> installed: $DEPLOY/bin/yardmaster"
"$DEPLOY/bin/yardmaster" version

echo "==> (re)starting deploy instance"
sh "$DEPLOY/run.sh"
