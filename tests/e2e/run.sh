#!/bin/sh
# Orchestrates the e2e run: build the plugin image, start it on a private
# network, then drive it with tests/e2e/cases.sh from a client container.
set -eu

cd "$(dirname "$0")/../.."
ROOT=$(pwd)

IMAGE=${IMAGE:-caddy-javascript:test}
NET=${NET:-caddy-sf-e2e-net}
SRV=${SRV:-caddy-sf-e2e-srv}
TOOL=${TOOL:-caddy:2.11.9-builder-alpine}
PORT=${PORT:-8080}
E2E="$ROOT/tests/e2e"

WORK=$(mktemp -d)

cleanup() {
	docker rm -f "$SRV" >/dev/null 2>&1 || true
	docker network rm "$NET" >/dev/null 2>&1 || true
	rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

if [ "${BUILD:-1}" = "1" ]; then
	"$ROOT/build.sh"
fi

cp -R "$E2E/app" "$WORK/app"
mkdir -p "$WORK/app/tmp"

docker network create "$NET" >/dev/null 2>&1 || true
docker rm -f "$SRV" >/dev/null 2>&1 || true

docker run -d --rm --name "$SRV" --network "$NET" \
	-e E2E_SECRET=s3cr3t \
	-v "$E2E/Caddyfile:/e2e/Caddyfile:ro" \
	-v "$WORK/app:/e2e/app" \
	"$IMAGE" run --config /e2e/Caddyfile --adapter caddyfile >/dev/null

status=0
docker run --rm --network "$NET" \
	-v "$E2E:/e2e:ro" \
	-v "$WORK/app:/work" \
	-e BASE_URL="http://$SRV:$PORT" \
	-e APP_DIR=/work \
	"$TOOL" sh /e2e/cases.sh || status=$?

if [ "$status" -ne 0 ]; then
	echo "--- server logs ---"
	docker logs "$SRV" 2>&1 || true
fi

exit "$status"
