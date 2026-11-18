#!/bin/sh
set -eu

cd "$(dirname "$0")/../.."
ROOT=$(pwd)

IMAGE=${IMAGE:-caddy-sf-e2e}
NET=${NET:-caddy-sf-e2e-net}
SRV=${SRV:-caddy-sf-e2e-srv}
MODVOL=${MODVOL:-caddy-sf-mod}
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
	docker build -t "$IMAGE" "$ROOT"
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
	-v "$ROOT:/src" -w /src \
	-v "$WORK/app:/work" \
	-v "$MODVOL:/mod" \
	-e GOMODCACHE=/mod -e GOCACHE=/mod/gocache -e CGO_ENABLED=0 \
	-e CADDY_E2E_ADDR="http://$SRV:$PORT" \
	-e CADDY_E2E_CASES=/src/tests/e2e/cases \
	-e CADDY_E2E_APP=/work \
	"$TOOL" go test -count=1 -v ./tests/e2e/ || status=$?

if [ "$status" -ne 0 ]; then
	echo "--- server logs ---"
	docker logs "$SRV" 2>&1 || true
fi

exit "$status"
