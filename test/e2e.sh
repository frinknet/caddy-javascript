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
	admin 127.0.0.1:2019
	persist_config off
	javascript /app/global.js
}

:80 {
	javascript /app/main.js {
		timeout 2s
		memory 67108864
		queue_size 8
	}
}

http://shared.example:80 {
	handle /site/* {
		javascript /app/shared-site.js
	}
	handle /api/* {
		javascript /app/shared-api.js
	}
	handle /surface/* {
		javascript /app/surface.js
	}
	handle /files/* {
		javascript /app/surface-rewrite.js
		file_server {
			root /app
		}
	}
	handle /template/* {
		javascript /app/template.js
	}
}

http://allow.example:80 {
	handle /template/* {
		javascript /app/template-allow.js {
			env_allow TEMPLATE_TEST
			fetch_allow 127.0.0.1
		}
	}
	handle /caps/* {
		javascript /app/caps-allow.js {
			fetch_allow 127.0.0.1
		}
	}
}

http://caps.example:80 {
	handle /caps/* {
		javascript /app/caps.js
	}
}

http://vars.example:80 {
	map {host} {site} {
		~^(.+?)\.\d{1,3}(?:[.-]\d{1,3}){3}\.(?:nip|sslip)\.io(?::\d+)?$ ${1}
		default {host}
	}
	vars from_caddy mapped
	handle {
		javascript /app/vars.js
		templates
		file_server {
			root /app
		}
	}
}

http://chain.example:80 {
	javascript /app/chain-auth.js
	javascript /app/chain-site.js
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
	-v "$TMP:/test:ro" \
	-v "$TMP/app:/app" \
	-e TEMPLATE_TEST=allowed \
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
#                      [--host NAME] [--cookie VALUE] [--rheader NAME:VALUE]
check() {
	check_name=$1
	check_method=$2
	check_path=$3
	shift 3

	check_status=
	check_equals=
	check_have_equals=0
	check_contains=
	check_host=
	check_cookie=
	check_rheader=

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
		--host)
			check_host=$2
			shift 2
			;;
		--cookie)
			check_cookie=$2
			shift 2
			;;
		--rheader)
			check_rheader=$2
			shift 2
			;;
		*)
			echo "check: unknown option $1" >&2
			exit 2
			;;
		esac
	done

	if [ "$check_method" = HEAD ]; then
		if [ -n "$check_host" ]; then
			check_got=$(curl -sS -o "$REQ/body" -D "$REQ/headers" -w '%{http_code}' --max-time 10 -H "Host: $check_host" -b "$check_cookie" -I "$BASE$check_path" 2>"$REQ/curl-err")
		else
			check_got=$(curl -sS -o "$REQ/body" -D "$REQ/headers" -w '%{http_code}' --max-time 10 -b "$check_cookie" -I "$BASE$check_path" 2>"$REQ/curl-err")
		fi
	else
		if [ -n "$check_host" ]; then
			check_got=$(curl -sS -o "$REQ/body" -D "$REQ/headers" -w '%{http_code}' --max-time 10 -H "Host: $check_host" -b "$check_cookie" -X "$check_method" "$BASE$check_path" 2>"$REQ/curl-err")
		else
			check_got=$(curl -sS -o "$REQ/body" -D "$REQ/headers" -w '%{http_code}' --max-time 10 -b "$check_cookie" -X "$check_method" "$BASE$check_path" 2>"$REQ/curl-err")
		fi
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
	if [ -n "$check_rheader" ]; then
		check_rname=${check_rheader%%:*}
		check_rvalue=${check_rheader#*:}
		check_rvalue=${check_rvalue# }
		if ! grep -qi "^$check_rname: *$check_rvalue" "$REQ/headers"; then
			echo "    response headers do not have $check_rname: $check_rvalue"
			check_bad=1
		fi
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

# wait_check NAME PATH [--host NAME] --equals BODY
# Retries until the body matches exactly, for work that settles on the loop.
wait_check() {
	wait_name=$1
	wait_path=$2
	shift 2
	wait_equals=
	wait_host=
	while [ "$#" -gt 0 ]; do
		case $1 in
		--equals)
			wait_equals=$2
			shift 2
			;;
		--host)
			wait_host=$2
			shift 2
			;;
		*)
			echo "wait_check: unknown option $1" >&2
			exit 2
			;;
		esac
	done

	i=0
	while [ "$i" -lt 50 ]; do
		if [ -n "$wait_host" ]; then
			curl -sS -o "$REQ/body" --max-time 5 -H "Host: $wait_host" "$BASE$wait_path" 2>"$REQ/curl-err"
		else
			curl -sS -o "$REQ/body" --max-time 5 "$BASE$wait_path" 2>"$REQ/curl-err"
		fi
		if [ "$(cat "$REQ/body")" = "$wait_equals" ]; then
			echo "  PASS: $wait_name"
			PASS=$((PASS + 1))
			return
		fi
		i=$((i + 1))
		sleep 0.1
	done
	echo "    body $(cat "$REQ/body"), want $wait_equals"
	echo "  FAIL: $wait_name"
	FAIL=$((FAIL + 1))
}

echo ""
echo "=== Test 1: site runtime ==="
check "site handler runs the entry module" GET / --status 200 --equals 'GET /'
check "site handler sees the method and path" POST /anything --status 200 --equals 'POST /anything'
check "global and site entries provision together" GET /again --status 200 --equals 'GET /again'

check "site handlers share one VM" GET /site/one --host shared.example --status 200 --equals 'site=1 lib=1'
check "handle directives share the site realm and module cache" GET /api/two --host shared.example --status 200 --equals 'api=1 lib=2'

check "route layers set request headers" GET "/surface/layer/hello?q=z" --host shared.example --cookie "c=1" --status 200 --equals 'GET|/surface/layer/hello|one|z|1' --rheader "x-layer: one"
check "http exposes query and cookies, and skips unmatched layers" GET /surface/plain --host shared.example --status 200 --equals 'GET|/surface/plain|none|-|-'
check "rewrite changes the path a later Caddy handler sees" GET /files/anything --host shared.example --status 200 --equals "$(cat test/app/lib.js)"
check "template sprig and pathEscape" GET /template/upper --host shared.example --status 200 --equals 'HELLO|a%20b'
check "template placeholder resolves the request host" GET /template/placeholder --host shared.example --status 200 --equals 'shared.example'
check "template readFile reads the sandbox" GET /template/read --host shared.example --status 200 --equals "$(cat test/app/lib.js)"
check "template fileExists distinguishes files" GET /template/exists --host shared.example --status 200 --equals 'true|false'
check "template listFiles lists the sandbox" GET /template/list --host shared.example --status 200 --equals 'yes'
check "template fileStat reports the file" GET /template/stat --host shared.example --status 200 --equals "lib.js|$(wc -c < test/app/lib.js | tr -d ' ')|false"
check "template humanize formats a size" GET /template/humanize --host shared.example --status 200 --equals '2.0 MB'
check "template splitFrontMatter splits metadata and body" GET /template/frontmatter --host shared.example --status 200 --equals 'Hi|Body'
check "template stripHTML" GET /template/html --host shared.example --status 200 --equals 'hi'
check "template markdown" GET /template/markdown --host shared.example --status 200 --equals '<h1>hi</h1>'
check "template httpStatus" GET /template/status --host shared.example --status 200 --equals 'Not Found'
check "template httpError sets the status" GET /template/error --host shared.example --status 404
check "template Date to time.Time" GET /template/epoch --host shared.example --status 200 --equals '1577923200'
check "template time.Time to RFC3339" GET /template/date --host shared.example --status 200 --equals '2020-01-02T00:00:00Z'
check "template include renders a request template" GET /template/include --host shared.example --status 200 --equals 'shared.example|shared.example'
check "template env is denied without env_allow" GET /template/env --host shared.example --status 500
check "template expandenv is denied without env_allow" GET /template/expandenv --host shared.example --status 500
check "template getHostByName is denied without fetch_allow" GET /template/dns --host shared.example --status 500
check "template httpInclude is denied without fetch_allow" GET /template/httpinclude --host shared.example --status 500
check "template readFile cannot escape the sandbox" GET /template/escape --host shared.example --status 500

check "template env is allowed by env_allow" GET /template/env --host allow.example --status 200 --equals 'allowed'
check "template expandenv is allowed by env_allow" GET /template/expandenv --host allow.example --status 200 --equals 'allowed'
check "template getHostByName is allowed by fetch_allow" GET /template/dns --host allow.example --status 200 --equals '127.0.0.1'
check "template httpInclude is allowed by fetch_allow" GET /template/httpinclude --host allow.example --status 200 --equals 'GET /anything'

check "bare globals and http.vars stay separate" GET /vars/page.html --host vars.example --status 200 --equals 'site=vars.example! host=vars.example caddy=mapped bare_site=vars.example! bare_fresh=new vars_explicit=new vars_fresh=[]'

check "a javascript directive that responds stops the chain" GET "/chain?token=no" --host chain.example --status 401 --equals 'unauthorized'
check "an unanswered javascript directive continues to the next one" GET "/chain?token=secret" --host chain.example --status 200 --equals 'token=secret'

echo ""
echo "=== Test 1a: capabilities ==="
check "fs reads the sandbox root" GET /caps/fs --host caps.example --status 200 --equals "$(cat test/app/lib.js)"
check "structuredClone copies plain data" GET /caps/clone --host caps.example --status 200 --equals '{"a":[1,2],"b":"x"}'
check "fetch is absent without fetch_allow" GET /caps/fetch --host caps.example --status 200 --equals 'undefined'
wait_check "a worker posts to its parent" /caps/worker --host caps.example --equals 'worker:ok'
wait_check "a BroadcastChannel reaches the same VM" /caps/channel --host caps.example --equals 'channel:ok'
wait_check "fetch reaches an allowed host" /caps/fetch --host allow.example --equals 'GET /anything'

echo ""
echo "=== Test 1b: worker config ==="
worker=0
i=0
while [ "$i" -lt 30 ]; do
	if [ "$(curl -s --max-time 2 "$BASE/worker/config")" = "worker" ]; then
		worker=1
		break
	fi
	i=$((i + 1))
	sleep 1
done
if [ "$worker" -eq 1 ]; then
	echo "  PASS: the worker added a native route through config.apply"
	PASS=$((PASS + 1))
else
	echo "  FAIL: the worker never added a native route"
	FAIL=$((FAIL + 1))
fi

state=""
i=0
while [ "$i" -lt 30 ]; do
	state=$(curl -s --max-time 2 "$BASE/worker/state")
	if [ "$state" = "calls=1" ]; then
		break
	fi
	i=$((i + 1))
	sleep 1
done
if [ "$state" = "calls=1" ]; then
	echo "  PASS: the worker survived its own reload"
	PASS=$((PASS + 1))
else
	echo "  FAIL: worker state after reload is $state"
	FAIL=$((FAIL + 1))
fi

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
echo "=== Test 3: reload rollback ==="

cat >"$TMP/reload-good.Caddyfile" <<'CADDY'
{
	admin 127.0.0.1:2019
	persist_config off
}
:80 {
	javascript /app/reload.js
}
CADDY

cat >"$TMP/reload-broken.Caddyfile" <<'CADDY'
{
	admin 127.0.0.1:2019
	persist_config off
}
:80 {
	javascript /app/broken.js
}
CADDY

if docker exec "$CONTAINER_NAME" caddy reload --config /test/reload-good.Caddyfile --adapter caddyfile >"$REQ/reload-out" 2>&1; then
	echo "  PASS: reload activates the new generation"
	PASS=$((PASS + 1))
else
	echo "  FAIL: reload rejected a valid configuration"
	sed 's/^/    /' "$REQ/reload-out"
	FAIL=$((FAIL + 1))
fi
check "reloaded generation serves the new module" GET / --status 200 --equals 'reloaded'

if docker exec "$CONTAINER_NAME" caddy reload --config /test/reload-broken.Caddyfile --adapter caddyfile >"$REQ/reload-out" 2>&1; then
	echo "  FAIL: reload accepted a module with a syntax error"
	FAIL=$((FAIL + 1))
else
	echo "  PASS: broken reload is rejected"
	PASS=$((PASS + 1))
fi
check "broken reload keeps the previous generation serving" GET / --status 200 --equals 'reloaded'

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
