#!/bin/sh
# End-to-end runner. Starts the image produced by ./build.sh with a mounted
# app, drives every documented endpoint over HTTP, and checks the results.
#
#   ./build.sh
#   E2E_IMAGE=caddy-javascript:test sh test/e2e.sh
#
# The image is not built here; E2E_IMAGE defaults to <repo>:test derived from
# NOTICE, which is what ./build.sh tags.

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

# ── App fixture ────────────────────────────────────────
# The app is copied so the reload check can rewrite the script without
# touching the checkout. The tmp directory is where fs.writeFile lands.
cp -R test/app "$TMP/app" || exit 1
mkdir -p "$TMP/app/tmp"
printf 'throw new Error("boom")\n' >"$TMP/app/throw.js"

# ── Caddyfile under test ───────────────────────────────
cat >"$TMP/Caddyfile" <<'CADDY'
{
	admin off
}

:80 {
	javascript /app/server.js {
		root        /app
		env_allow   E2E_SECRET
		fetch_allow 127.0.0.1
	}
}
CADDY

# ── Test 0: the image runs the versions go.mod pins ────
# A newer driver in the image is exactly how a dependency mismatch slips
# through unit tests, which resolve from go.mod.
echo ""
echo "=== Test 0: dependency versions ==="
for mod in "github.com/caddyserver/caddy/v2" "github.com/grafana/sobek"; do
	want=$(awk -v m="$mod" '$1 == m && $2 ~ /^v/ {print $2}' go.mod)
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
	-e E2E_SECRET=s3cr3t \
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
	if curl -sf -o /dev/null --max-time 2 "$BASE/"; then
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

# check NAME METHOD PATH [--status N] [--ctype TYPE] [--equals BODY]
#     [--contains TEXT] [--header 'Name: value'] [--hdr Name Value]
#     [--hdr-contains Name Text] [--data BODY] [--data-file FILE]
#
# --equals is an exact body comparison. Everything else is a match.
check() {
	check_name=$1
	check_method=$2
	check_path=$3
	shift 3

	check_status=
	check_ctype=
	check_equals=
	check_have_equals=0
	check_contains=
	check_headers=$REQ/req-headers
	check_want_headers=$REQ/want-headers
	check_want_contains=$REQ/want-headers-contains
	check_data=
	check_data_file=
	: >"$check_headers"
	: >"$check_want_headers"
	: >"$check_want_contains"

	while [ "$#" -gt 0 ]; do
		case $1 in
		--status)
			check_status=$2
			shift 2
			;;
		--ctype)
			check_ctype=$2
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
		--header)
			printf '%s\n' "$2" >>"$check_headers"
			shift 2
			;;
		--hdr)
			printf '%s: %s\n' "$2" "$3" >>"$check_want_headers"
			shift 3
			;;
		--hdr-contains)
			printf '%s: %s\n' "$2" "$3" >>"$check_want_contains"
			shift 3
			;;
		--data)
			check_data=$2
			shift 2
			;;
		--data-file)
			check_data_file=$2
			shift 2
			;;
		*)
			echo "check: unknown option $1" >&2
			exit 2
			;;
		esac
	done

	set -- -sS -o "$REQ/body" -D "$REQ/hdrs" -w '%{http_code}' --max-time 10
	if [ "$check_method" = HEAD ]; then
		set -- "$@" -I
	else
		set -- "$@" -X "$check_method"
	fi
	if [ -n "$check_data" ]; then
		printf '%s' "$check_data" >"$REQ/req-body"
		set -- "$@" --data-binary @"$REQ/req-body"
	elif [ -n "$check_data_file" ]; then
		set -- "$@" --data-binary @"$check_data_file"
	fi
	while IFS= read -r line; do
		[ -n "$line" ] && set -- "$@" -H "$line"
	done <"$check_headers"
	set -- "$@" "$BASE$check_path"

	check_got=$(curl "$@" 2>"$REQ/curl-err")
	check_bad=0

	if [ -n "$check_status" ] && [ "$check_got" != "$check_status" ]; then
		echo "    status $check_got, want $check_status"
		check_bad=1
	fi
	if [ -n "$check_ctype" ] && ! tr -d '\r' <"$REQ/hdrs" | grep -qi "^content-type:[[:space:]]*.*$check_ctype"; then
		echo "    content-type $(tr -d '\r' <"$REQ/hdrs" | grep -i '^content-type:' | head -n 1), want to contain $check_ctype"
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
	while IFS= read -r line; do
		[ -n "$line" ] || continue
		check_hname=${line%%:*}
		check_hvalue=${line#*: }
		if ! tr -d '\r' <"$REQ/hdrs" | grep -qi "^$check_hname:[[:space:]]*$check_hvalue$"; then
			echo "    header $check_hname $(tr -d '\r' <"$REQ/hdrs" | grep -i "^$check_hname:" | head -n 1), want $check_hvalue"
			check_bad=1
		fi
	done <"$check_want_headers"
	while IFS= read -r line; do
		[ -n "$line" ] || continue
		check_hname=${line%%:*}
		check_hvalue=${line#*: }
		if ! tr -d '\r' <"$REQ/hdrs" | grep -qi "^$check_hname:[[:space:]]*.*$check_hvalue"; then
			echo "    header $check_hname $(tr -d '\r' <"$REQ/hdrs" | grep -i "^$check_hname:" | head -n 1), want to contain $check_hvalue"
			check_bad=1
		fi
	done <"$check_want_contains"
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

# ── Test 1: basic request and response ─────────────────
echo ""
echo "=== Test 1: basic ==="
check "root returns json" GET / --status 200 --ctype application/json --contains '"ok":true' --hdr X-Global yes
check "hello returns text" GET /hello --status 200 --ctype text/plain --equals 'hello world'
check "echo returns created json" POST /echo --data '{"a":1}' --header 'Content-Type: application/json' --status 201 --ctype application/json --contains '"a":1'
check "query is parsed" GET '/query?a=1&b=2' --status 200 --ctype application/json --contains '"a":"1"'
check "queryAll returns every value" GET '/queryall?a=1&a=2' --status 200 --ctype application/json --contains '"a":["1","2"]'
check "unknown path falls back to 404" GET /missing --status 404 --contains 'not found'

# ── Test 2: response fidelity ──────────────────────────
echo ""
echo "=== Test 2: response fidelity ==="
check "res.set writes a header" GET /header --status 200 --equals ok --hdr X-Custom yes
check "res.type sets content type" GET /type --status 200 --ctype text/csv --equals 'a,b'
check "res.redirect sends a location" GET /redirect --status 302 --hdr Location /hello
check "res.end sends no body" GET /end --status 204 --equals ''
check "res.append adds repeated header values" GET /append --status 200 --equals ok --hdr X-Multi one --hdr-contains X-Multi three
check "res.location sets a relative location without redirecting" GET /location --status 201 --equals ok --hdr Location /hello
check "res.location('back') uses the referer" GET /location-back --header 'Referer: http://example.test/back' --status 303 --equals ok --hdr Location http://example.test/back
check "res.cookie sets an encoded cookie with options" GET /cookie --status 200 --equals ok --hdr-contains Set-Cookie 'sid=a%20b' --hdr-contains Set-Cookie 'Path=/app' --hdr-contains Set-Cookie 'Max-Age=60' --hdr-contains Set-Cookie 'HttpOnly' --hdr-contains Set-Cookie 'SameSite=Lax'
check "res.sendStatus sends the status text" GET /sendstatus --status 418 --ctype text/plain --equals "I'm a teapot"
check "res.flush keeps the response writable" GET /flush --status 200 --ctype text/plain --equals flushed
check "res.sendFile serves a rooted file with headers and max-age" GET /sendfile --status 200 --ctype text/plain --contains 'static hello' --hdr X-File yes --hdr Cache-Control 'public, max-age=60'
check "res.sendFile answers HEAD with headers only" HEAD /sendfile-head --status 200 --ctype text/plain --hdr Content-Length 12
check "res.sendFile on a missing file is a 404" GET /sendfile-missing --status 404
check "res.sendFile denies dotfiles when asked" GET /sendfile-dotfile --status 403
check "res.sendFile cannot escape its root" GET /sendfile-escape --status 404

# ── Test 3: request parsing ────────────────────────────
echo ""
echo "=== Test 3: request parsing ==="
check "urlencoded body is parsed" POST /form --data 'a=1&b=2' --header 'Content-Type: application/x-www-form-urlencoded' --status 200 --ctype application/json --contains '"a":"1"'
check "urlencoded repeated field keeps every value" POST /form --data 'a=1&a=2&b=3' --header 'Content-Type: application/x-www-form-urlencoded' --status 200 --ctype application/json --contains '"a":["1","2"]'
printf '%b' '\055-X\r\nContent-Disposition: form-data; name="a"\r\n\r\n1\r\n\055-X\r\nContent-Disposition: form-data; name="b"\r\n\r\n2\r\n\055-X--\r\n' >"$REQ/multipart"
check "multipart body is parsed" POST /form --data-file "$REQ/multipart" --header 'Content-Type: multipart/form-data; boundary=X' --status 200 --ctype application/json --contains '"a":"1"'
check "req.header reads a request header" GET /echoheader --header 'X-Test: hello' --status 200 --equals hello

# ── Test 4: express routing ────────────────────────────
echo ""
echo "=== Test 4: express routing ==="
check "require('express') factory registers a route" GET /express/hello --status 200 --equals 'express hello'
check "app.put matches a PUT request" PUT /express/put --status 200 --equals 'put ok'
check "app.patch matches a PATCH request" PATCH /express/patch --status 200 --equals 'patch ok'
check "app.delete matches a DELETE request" DELETE /express/delete --status 200 --equals 'delete ok'
check "app.all matches every method" GET /express/any --status 200 --equals GET
check "app.all matches POST" POST /express/any --status 200 --equals POST
check "app.options matches OPTIONS" OPTIONS /express/options --status 200 --hdr Allow 'GET, PUT, DELETE'
check "app.head matches a HEAD request" HEAD /express/head --status 200 --hdr X-Head yes
check "express.Router() registers routes relative to its mount" GET /api/hello --status 200 --equals 'router hello'
check "mounted router captures params" GET /api/user/9 --status 200 --ctype application/json --equals '{"id":"9"}'
check "router.route chains methods on one path" GET /api/chain/5 --status 200 --equals '{"id":"5"}'
check "router.route POST shares the path" POST /api/chain/5 --status 201 --equals '{"created":"5"}'
check "router.param runs before the route handler" GET /param/item/3 --status 200 --equals '{"id":"3","seen":"3"}'
check "express.static serves a file" GET /static/hello.txt --status 200 --ctype text/plain --contains 'static hello'
check "express.static serves a directory index" GET /static/ --status 200 --ctype text/html --contains 'static index'
check "express.static misses fall through" GET /static/missing.txt --status 404 --contains 'not found'
check "express.static ignores dotfiles by default" GET /static/.secret --status 404 --contains 'not found'
check "mounted middleware runs before the route" GET /mw/hello --status 200 --equals 'mw hello' --hdr X-MW yes
check "route middleware runs before its handler" GET /mw/route --status 200 --equals 'route mw' --hdr X-Route-MW 1
check "path parameter is captured" GET /users/42 --status 200 --ctype application/json --equals '{"id":"42"}'
check "two path parameters are captured" GET /users/42/posts/7 --status 200 --equals '{"id":"42","post":"7"}'
check "wildcard captures the rest of the path" GET /files/a/b.txt --status 200 --equals 'a/b.txt'
check "next(err) reaches the error handler" GET /boom --status 503 --equals 'handled: kaboom'
check "an unhandled throw is a 500" GET /throw --status 500
check "matching is case-insensitive" GET /Hello --status 200 --equals 'hello world'
check "matching ignores a trailing slash" GET /hello/ --status 200 --equals 'hello world'

# ── Test 5: fetch ──────────────────────────────────────
echo ""
echo "=== Test 5: fetch ==="
check "callback fetch returns the response text" GET /fetch-allowed --status 200 --contains '"text":"hello world"'
check "promise fetch returns the response text" GET /fetch/promise --status 200 --ctype application/json --equals '{"status":200,"text":"hello world"}'
check "fetch exposes response headers and status" GET /fetch/headers --status 200 --contains '"custom":"yes"'
check "fetch sends method, headers, and body" GET /fetch/post --status 200 --contains '"a":1'
check "fetch streams the response body" GET /fetch/stream --status 200 --equals 'hello world'
check "fetch rejects a blocked host" GET /fetch/reject --status 502 --contains 'not in fetch_allow'
check "callback fetch rejects a blocked host" GET /fetch-blocked --status 403 --equals blocked

# ── Test 6: modules, fs, path, timers ──────────────────
echo ""
echo "=== Test 6: modules, fs, path, timers ==="
check "relative require" GET /module/relative --status 200 --ctype text/plain --equals 'hello world'
check "node_modules require" GET /module/node-modules --status 200 --equals 07
check "json require" GET /module/json --status 200 --ctype application/json --contains '"name":"e2e-app"'
check "fs.readFile reads inside the root" GET /fs/read --status 200 --equals 'static hello'
check "fs.writeFile writes inside the root" GET /fs/write --status 200 --equals ok
check "fs.readFileSync reads what was written" GET /fs/read-written --status 200 --equals written
check "absolute-looking paths resolve inside the root" GET /fs/absolute --status 200 --equals 'static hello'
check "parent segments are clamped to the root" GET /fs/clamp --status 200 --equals 'static hello'
check "a missing fs file reports an error" GET /fs/missing --status 500 --contains 'no such file'
check "path.join uses POSIX semantics" GET /path/join --status 200 --equals 'a/c'
check "setTimeout completes the response" GET /timer --status 200 --equals 'timer fired'
check "setInterval and clearInterval complete the response" GET /interval --status 200 --equals 'interval 2'

# ── Test 7: security ───────────────────────────────────
echo ""
echo "=== Test 7: security ==="
check "env allowlist exposes the listed variable" GET /env --status 200 --ctype application/json --contains '"secret":"s3cr3t"'
check "env allowlist hides everything else" GET /env --status 200 --contains '"home":null'

# ── Test 8: reload ─────────────────────────────────────
echo ""
echo "=== Test 8: reload ==="
check "version starts at v1" GET /version --status 200 --equals v1
reload_bad=0
if sed -i "s/res.send('v1')/res.send('v2')/" "$TMP/app/server.js"; then
	i=0
	while [ "$i" -lt 75 ]; do
		if [ "$(curl -sS --max-time 2 "$BASE/version" 2>/dev/null)" = v2 ]; then
			break
		fi
		i=$((i + 1))
		sleep 0.2
	done
	if [ "$(curl -sS --max-time 2 "$BASE/version" 2>/dev/null)" = v2 ]; then
		echo "  PASS: script is reloaded after the file changes"
	else
		echo "  FAIL: script was not reloaded after the file changed"
		reload_bad=1
	fi
else
	echo "  FAIL: could not rewrite the script for the reload check"
	reload_bad=1
fi
if [ "$reload_bad" -eq 0 ]; then
	PASS=$((PASS + 1))
else
	FAIL=$((FAIL + 1))
fi

# ── Test 9: provision-time validation ──────────────────
echo ""
echo "=== Test 9: provision-time validation ==="

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
	javascript /app/server.js /app/lib/greet.js
}
CADDY

cat >"$TMP/neg-unknown.Caddyfile" <<'CADDY'
{
	admin off
}
:8080 {
	javascript /app/server.js {
		use app
	}
}
CADDY

cat >"$TMP/neg-queue.Caddyfile" <<'CADDY'
{
	admin off
}
:8080 {
	javascript /app/server.js {
		queue_size -1
	}
}
CADDY

cat >"$TMP/neg-max-body.Caddyfile" <<'CADDY'
{
	admin off
}
:8080 {
	javascript /app/server.js {
		max_body -1
	}
}
CADDY

cat >"$TMP/neg-timeout.Caddyfile" <<'CADDY'
{
	admin off
}
:8080 {
	javascript /app/server.js {
		timeout nope
	}
}
CADDY

cat >"$TMP/neg-root.Caddyfile" <<'CADDY'
{
	admin off
}
:8080 {
	javascript /app/server.js {
		root
	}
}
CADDY

cat >"$TMP/neg-watch.Caddyfile" <<'CADDY'
{
	admin off
}
:8080 {
	javascript /app/server.js {
		watch nope
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

cat >"$TMP/neg-throwing-script.Caddyfile" <<'CADDY'
{
	admin off
}
:8080 {
	javascript /app/throw.js
}
CADDY

neg "missing script path fails" "$TMP/neg-missing.Caddyfile" 'missing script path'
neg "too many arguments fail" "$TMP/neg-too-many.Caddyfile" 'too many arguments'
neg "unknown option fails" "$TMP/neg-unknown.Caddyfile" 'unrecognized option'
neg "queue_size floor fails" "$TMP/neg-queue.Caddyfile" 'queue_size must be at least 1'
neg "negative max_body fails" "$TMP/neg-max-body.Caddyfile" 'max_body must not be negative'
neg "invalid timeout fails" "$TMP/neg-timeout.Caddyfile" 'invalid duration'
neg "missing root argument fails" "$TMP/neg-root.Caddyfile" 'javascript root: missing path'
neg "invalid watch value fails" "$TMP/neg-watch.Caddyfile" 'invalid syntax'
neg "missing script file fails" "$TMP/neg-missing-script.Caddyfile" 'failed to initialize runtime'
neg "script that throws at load fails" "$TMP/neg-throwing-script.Caddyfile" 'failed to initialize runtime'

# ── Summary ────────────────────────────────────────────
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
