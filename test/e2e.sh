#!/bin/sh
# End-to-end runner. Starts the image produced by ./build.sh, checks the new
# configuration surface, and checks that invalid configurations fail validation.
#
#   ./build.sh
#   E2E_IMAGE=caddy-javascript:test sh test/e2e.sh

set -u

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT" || exit 1

TMP=$(mktemp -d "${TMPDIR:-/tmp}/caddy-javascript-e2e-XXXXXX")
REQ="$TMP/req"
PORT=${PORT:-9099}
BASE="http://127.0.0.1:$PORT"
SLUG=$(sed -n '1s/^[^:]*: //p' ./NOTICE)
IMAGE=${E2E_IMAGE:-${SLUG}:test}
CONTAINER_NAME="caddy-javascript-e2e-runner"
PASS=0
FAIL=0

cleanup() {
	docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
	rm -rf "$TMP"
}
trap cleanup EXIT INT TERM

mkdir -p "$REQ"

if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
	echo "image $IMAGE not found; run ./build.sh first" >&2
	exit 1
fi

cp -R test/app "$TMP/app" || exit 1

cat >"$TMP/Caddyfile" <<'CADDY'
{
	admin off
	javascript /app/global.js
}

:80 {
	javascript /app/main.js {
		timeout 2s
		memory 67108864
		queue_size 8
	}
}
CADDY

echo ""
echo "=== Test 0: image dependency versions ==="
for mod in "github.com/caddyserver/caddy/v2" "github.com/go-quickjs/go-quickjs"; do
	want=$(awk -v m="$mod" '{ for (i = 1; i < NF; i++) if ($i == m && $(i + 1) ~ /^v/) { print $(i + 1); exit } }' go.mod)
	got=$(docker run --rm --entrypoint /bin/cat "$IMAGE" /usr/share/licenses/versions.txt 2>/dev/null \
		| awk -v m="$mod" '$1 == "dep" && $2 == m {print $3}')
	if [ -n "$want" ] && [ "$want" = "$got" ]; then
		echo "  PASS: $mod $got matches go.mod"
		PASS=$((PASS + 1))
	else
		echo "  FAIL: $mod: go.mod pins ${want:-nothing}, image has ${got:-nothing}"
		FAIL=$((FAIL + 1))
	fi
done

echo ""
echo "=== Starting Caddy container ==="
docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
if ! docker run -d \
	--name "$CONTAINER_NAME" \
	-v "$TMP/Caddyfile:/etc/caddy/Caddyfile:ro" \
	-v "$TMP/app:/app" \
	-p "127.0.0.1:$PORT:80" \
	"$IMAGE" run --config /etc/caddy/Caddyfile --adapter caddyfile >/dev/null 2>&1; then
	echo "failed to start container from $IMAGE" >&2
	docker logs "$CONTAINER_NAME" 2>&1 || true
	exit 1
fi

echo "=== Waiting for Caddy ==="
ready=0
i=0
while [ "$i" -lt 30 ]; do
	if curl -s -o /dev/null --max-time 2 "$BASE/"; then
		ready=1
		echo "Caddy ready after ${i}s"
		break
	fi
	i=$((i + 1))
	sleep 1
done
if [ "$ready" -ne 1 ]; then
	echo "Caddy never became ready at $BASE" >&2
	docker logs "$CONTAINER_NAME" 2>&1 || true
	exit 1
fi

# check NAME METHOD PATH [--status N] [--equals BODY] [--contains TEXT]
check() {
	check_name=$1
	check_method=$2
	check_path=$3
	shift 3

	check_status=
	check_equals=
	check_have_equals=0
	check_contains=

	while [ "$#" -gt 0 ]; do
		case $1 in
		--status)
			check_status=$2
			shift 2
			;;
		--equals)
			check_equals=$2
			check_have_equals=1
			shift 2
			;;
		--contains)
			check_contains=$2
			shift 2
			;;
		*)
			echo "check: unknown option $1" >&2
			exit 2
			;;
		esac
	done

	if [ "$check_method" = HEAD ]; then
		check_got=$(curl -sS -o "$REQ/body" -w '%{http_code}' --max-time 10 -I "$BASE$check_path" 2>"$REQ/curl-err")
	else
		check_got=$(curl -sS -o "$REQ/body" -w '%{http_code}' --max-time 10 -X "$check_method" "$BASE$check_path" 2>"$REQ/curl-err")
	fi
	check_bad=0

	if [ -n "$check_status" ] && [ "$check_got" != "$check_status" ]; then
		echo "    status $check_got, want $check_status"
		check_bad=1
	fi
	if [ "$check_have_equals" -eq 1 ] && [ "$(cat "$REQ/body")" != "$check_equals" ]; then
		echo "    body $(cat "$REQ/body"), want $check_equals"
		check_bad=1
	fi
	if [ -n "$check_contains" ] && ! grep -qF -- "$check_contains" "$REQ/body"; then
		echo "    body $(cat "$REQ/body"), want to contain $check_contains"
		check_bad=1
	fi
	if [ -s "$REQ/curl-err" ]; then
		echo "    curl: $(cat "$REQ/curl-err")"
		check_bad=1
	fi

	if [ "$check_bad" -eq 0 ]; then
		echo "  PASS: $check_name"
		PASS=$((PASS + 1))
	else
		echo "  FAIL: $check_name"
		FAIL=$((FAIL + 1))
	fi
}

echo ""
echo "=== Test 1: site runtime ==="
check "site handler runs the entry module" GET / --status 200 --equals 'GET /'
check "site handler sees the method and path" POST /anything --status 200 --equals 'POST /anything'
check "global and site entries provision together" GET /again --status 200 --equals 'GET /again'

echo ""
echo "=== Test 2: configuration validation ==="
neg() {
	neg_desc=$1
	neg_file=$2
	neg_expect=$3
	neg_out=$(docker run --rm \
		-v "$neg_file:/etc/caddy/Caddyfile:ro" \
		-v "$TMP/app:/app:ro" \
		"$IMAGE" validate --config /etc/caddy/Caddyfile --adapter caddyfile 2>&1)
	neg_status=$?
	if [ "$neg_status" -ne 0 ] && echo "$neg_out" | grep -q "$neg_expect"; then
		echo "  PASS: $neg_desc"
		PASS=$((PASS + 1))
	else
		echo "  FAIL: $neg_desc"
		echo "    expected non-zero exit containing: $neg_expect"
		echo "    got exit=$neg_status: $neg_out"
		FAIL=$((FAIL + 1))
	fi
}

cat >"$TMP/neg-missing.Caddyfile" <<'CADDY'
{
	admin off
}
:8080 {
	javascript
}
CADDY

cat >"$TMP/neg-too-many.Caddyfile" <<'CADDY'
{
	admin off
}
:8080 {
	javascript /app/main.js /app/global.js
}
CADDY

cat >"$TMP/neg-unknown.Caddyfile" <<'CADDY'
{
	admin off
}
:8080 {
	javascript /app/main.js {
		use app
	}
}
CADDY

cat >"$TMP/neg-site.Caddyfile" <<'CADDY'
{
	admin off
}
:8080 {
	javascript /app/main.js {
		site
	}
}
CADDY

cat >"$TMP/neg-timeout.Caddyfile" <<'CADDY'
{
	admin off
}
:8080 {
	javascript /app/main.js {
		timeout nope
	}
}
CADDY

cat >"$TMP/neg-memory.Caddyfile" <<'CADDY'
{
	admin off
}
:8080 {
	javascript /app/main.js {
		memory -1
	}
}
CADDY

cat >"$TMP/neg-queue.Caddyfile" <<'CADDY'
{
	admin off
}
:8080 {
	javascript /app/main.js {
		queue_size -1
	}
}
CADDY

cat >"$TMP/neg-missing-script.Caddyfile" <<'CADDY'
{
	admin off
}
:8080 {
	javascript /app/nope.js
}
CADDY

cat >"$TMP/neg-global-script.Caddyfile" <<'CADDY'
{
	admin off
	javascript /app/nope-global.js
}
:8080 {
	javascript /app/main.js
}
CADDY

cat >"$TMP/neg-duplicate-global.Caddyfile" <<'CADDY'
{
	admin off
	javascript /app/global.js
	javascript /app/global.js
}
:8080 {
	javascript /app/main.js
}
CADDY

neg "missing script path fails" "$TMP/neg-missing.Caddyfile" 'missing script path'
neg "too many arguments fail" "$TMP/neg-too-many.Caddyfile" 'too many arguments'
neg "unknown option fails" "$TMP/neg-unknown.Caddyfile" 'unrecognized option'
neg "missing site value fails" "$TMP/neg-site.Caddyfile" 'javascript site: missing value'
neg "invalid timeout fails" "$TMP/neg-timeout.Caddyfile" 'invalid duration'
neg "negative memory fails" "$TMP/neg-memory.Caddyfile" 'memory must not be negative'
neg "queue_size floor fails" "$TMP/neg-queue.Caddyfile" 'queue_size must be at least 1'
neg "missing script file fails" "$TMP/neg-missing-script.Caddyfile" 'no such file'
neg "missing global script fails" "$TMP/neg-global-script.Caddyfile" 'no such file'
neg "duplicate global option fails" "$TMP/neg-duplicate-global.Caddyfile" 'may only be set once'

echo ""
echo "=========================================="
echo "  PASS: $PASS   FAIL: $FAIL"
echo "=========================================="
if [ "$FAIL" -eq 0 ]; then
	echo "ALL TESTS PASSED"
else
	echo "SOME TESTS FAILED"
fi
exit "$FAIL"
