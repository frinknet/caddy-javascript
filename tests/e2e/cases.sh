#!/bin/sh
# Runs inside a client container next to the e2e server. Assertions are plain
# shell and curl; the orchestration lives in run.sh.
set -u

BASE=${BASE_URL:?BASE_URL is required}
APP=${APP_DIR:-/work}

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0

wait_ready() {
	i=0
	while [ "$i" -lt 60 ]; do
		if curl -fsS -o /dev/null --max-time 2 "$BASE/" 2>/dev/null; then
			return 0
		fi
		i=$((i + 1))
		sleep 1
	done
	echo "server at $BASE never became ready" >&2
	exit 1
}

# check NAME METHOD PATH [--status N] [--ctype TYPE] [--equals BODY]
#     [--contains TEXT] [--header 'Name: value'] [--hdr Name Value]
#     [--data BODY] [--data-file FILE]
check() {
	check_name=$1 check_method=$2 check_path=$3
	shift 3

	want_status=
	want_ctype=
	want_equals=
	have_equals=0
	want_contains=
	req_headers=$TMP/req-headers
	want_headers=$TMP/want-headers
	data=
	data_file=
	: >"$req_headers"
	: >"$want_headers"

	while [ "$#" -gt 0 ]; do
		case $1 in
		--status)
			want_status=$2
			shift 2
			;;
		--ctype)
			want_ctype=$2
			shift 2
			;;
		--equals)
			want_equals=$2
			have_equals=1
			shift 2
			;;
		--contains)
			want_contains=$2
			shift 2
			;;
		--header)
			printf '%s\n' "$2" >>"$req_headers"
			shift 2
			;;
		--hdr)
			printf '%s: %s\n' "$2" "$3" >>"$want_headers"
			shift 3
			;;
		--data)
			data=$2
			shift 2
			;;
		--data-file)
			data_file=$2
			shift 2
			;;
		*)
			echo "check: unknown option $1" >&2
			exit 2
			;;
		esac
	done

	set -- -sS -o "$TMP/body" -D "$TMP/hdrs" -w '%{http_code}' --max-time 10
	if [ "$check_method" = HEAD ]; then
		set -- "$@" -I
	else
		set -- "$@" -X "$check_method"
	fi
	if [ -n "$data" ]; then
		printf '%s' "$data" >"$TMP/req-body"
		set -- "$@" --data-binary @"$TMP/req-body"
	elif [ -n "$data_file" ]; then
		set -- "$@" --data-binary @"$data_file"
	fi
	while IFS= read -r line; do
		[ -n "$line" ] && set -- "$@" -H "$line"
	done <"$req_headers"
	set -- "$@" "$BASE$check_path"

	status=$(curl "$@" 2>"$TMP/curl-err")
	bad=0

	if [ -n "$want_status" ] && [ "$status" != "$want_status" ]; then
		echo "  status $status, want $want_status"
		bad=1
	fi
	if [ -n "$want_ctype" ] && ! tr -d '\r' <"$TMP/hdrs" | grep -qi "^content-type:[[:space:]]*.*$want_ctype"; then
		echo "  content-type $(tr -d '\r' <"$TMP/hdrs" | grep -i '^content-type:' | head -n 1), want to contain $want_ctype"
		bad=1
	fi
	if [ "$have_equals" = 1 ] && [ "$(cat "$TMP/body")" != "$want_equals" ]; then
		echo "  body $(cat "$TMP/body"), want $want_equals"
		bad=1
	fi
	if [ -n "$want_contains" ] && ! grep -qF -- "$want_contains" "$TMP/body"; then
		echo "  body $(cat "$TMP/body"), want to contain $want_contains"
		bad=1
	fi
	while IFS= read -r line; do
		[ -n "$line" ] || continue
		name=${line%%:*}
		value=${line#*: }
		if ! tr -d '\r' <"$TMP/hdrs" | grep -qi "^$name:[[:space:]]*$value$"; then
			echo "  header $name $(tr -d '\r' <"$TMP/hdrs" | grep -i "^$name:" | head -n 1), want $value"
			bad=1
		fi
	done <"$want_headers"
	if [ -s "$TMP/curl-err" ]; then
		echo "  curl: $(cat "$TMP/curl-err")"
		bad=1
	fi

	if [ "$bad" -eq 0 ]; then
		pass=$((pass + 1))
		echo "ok   $check_name"
	else
		fail=$((fail + 1))
		echo "FAIL $check_name"
	fi
}

wait_ready

echo "== basic"
check "root returns json" GET / --status 200 --ctype application/json --contains '"ok":true'
check "hello returns text" GET /hello --status 200 --ctype text/plain --equals 'hello world'
check "echo returns created json" POST /echo --data '{"a":1}' --header 'Content-Type: application/json' --status 201 --ctype application/json --contains '"a":1'
check "query is parsed" GET '/query?a=1&b=2' --status 200 --ctype application/json --contains '"a":"1"'
check "unknown path falls back to 404" GET /missing --status 404 --contains 'not found'

echo "== express"
check "require('express') factory registers a route" GET /express/hello --status 200 --equals 'express hello'
check "app.put matches a PUT request" PUT /express/put --status 200 --equals 'put ok'
check "app.delete matches a DELETE request" DELETE /express/delete --status 200 --equals 'delete ok'
check "app.all matches every method" GET /express/any --status 200 --equals GET
check "app.all matches POST" POST /express/any --status 200 --equals POST
check "app.head matches a HEAD request" HEAD /express/head --status 200 --hdr X-Head yes
check "express.Router() registers routes relative to its mount" GET /api/hello --status 200 --equals 'router hello'
check "mounted router captures params" GET /api/user/9 --status 200 --ctype application/json --equals '{"id":"9"}'
check "express.static serves a file" GET /static/hello.txt --status 200 --ctype text/plain --contains 'static hello'
check "express.static serves a directory index" GET /static/ --status 200 --ctype text/html --contains 'static index'
check "express.static misses fall through" GET /static/missing.txt --status 404 --contains 'not found'
check "mounted middleware runs before the route" GET /mw/hello --status 200 --equals 'mw hello' --hdr X-MW yes
check "route middleware runs before its handler" GET /mw/route --status 200 --equals 'route mw' --hdr X-Route-MW 1
check "path parameter is captured" GET /users/42 --status 200 --ctype application/json --equals '{"id":"42"}'
check "two path parameters are captured" GET /users/42/posts/7 --status 200 --equals '{"id":"42","post":"7"}'
check "wildcard captures the rest of the path" GET /files/a/b.txt --status 200 --equals 'a/b.txt'
check "next(err) reaches the error handler" GET /boom --status 503 --equals 'handled: kaboom'
check "an unhandled throw is a 500" GET /throw --status 500
check "matching is case-insensitive" GET /Hello --status 200 --equals 'hello world'
check "matching ignores a trailing slash" GET /hello/ --status 200 --equals 'hello world'

echo "== fetch"
check "promise fetch returns the response text" GET /fetch/promise --status 200 --ctype application/json --equals '{"status":200,"text":"hello world"}'
check "fetch exposes response headers and status" GET /fetch/headers --status 200 --contains '"custom":"yes"'
check "fetch sends method, headers, and body" GET /fetch/post --status 200 --contains '"a":1'
check "fetch streams the response body" GET /fetch/stream --status 200 --equals 'hello world'
check "fetch rejects a blocked host" GET /fetch/reject --status 502 --contains 'not in fetch_allow'

echo "== fidelity"
check "queryAll returns every value" GET '/queryall?a=1&a=2' --status 200 --ctype application/json --contains '"a":["1","2"]'
check "res.set writes a header" GET /header --status 200 --equals ok --hdr X-Custom yes
check "res.type sets content type" GET /type --status 200 --ctype text/csv --equals 'a,b'
check "res.redirect sends a location" GET /redirect --status 302 --hdr Location /hello
check "res.end sends no body" GET /end --status 204 --equals ''
check "urlencoded body is parsed" POST /form --data 'a=1&b=2' --header 'Content-Type: application/x-www-form-urlencoded' --status 200 --ctype application/json --contains '"a":"1"'
check "urlencoded repeated field keeps every value" POST /form --data 'a=1&a=2&b=3' --header 'Content-Type: application/x-www-form-urlencoded' --status 200 --ctype application/json --contains '"a":["1","2"]'
check "req.header reads a request header" GET /echoheader --header 'X-Test: hello' --status 200 --equals hello
printf '%b' '\055-X\r\nContent-Disposition: form-data; name="a"\r\n\r\n1\r\n\055-X\r\nContent-Disposition: form-data; name="b"\r\n\r\n2\r\n\055-X--\r\n' >"$TMP/multipart"
check "multipart body is parsed" POST /form --data-file "$TMP/multipart" --header 'Content-Type: multipart/form-data; boundary=X' --status 200 --ctype application/json --contains '"a":"1"'

echo "== modules"
check "relative require" GET /module/relative --status 200 --ctype text/plain --equals 'hello world'
check "node_modules require" GET /module/node-modules --status 200 --equals 07
check "json require" GET /module/json --status 200 --ctype application/json --contains '"name":"e2e-app"'

echo "== security"
check "env allowlist exposes the listed variable" GET /env --status 200 --ctype application/json --contains '"secret":"s3cr3t"'
check "env allowlist hides everything else" GET /env --status 200 --contains '"home":null'
check "fetch to an allowlisted host works" GET /fetch-allowed --status 200 --contains '"text":"hello world"'
check "fetch to a non-allowlisted host is blocked" GET /fetch-blocked --status 403 --equals blocked

echo "== reload"
check "version starts at v1" GET /version --status 200 --equals v1
reload_bad=0
if grep -q "res.send('v1')" "$APP/server.js"; then
	sed -i "s/res.send('v1')/res.send('v2')/" "$APP/server.js"
else
	echo "  did not find the version route to rewrite"
	reload_bad=1
fi
if [ "$reload_bad" -eq 0 ]; then
	i=0
	while [ "$i" -lt 75 ]; do
		if [ "$(curl -fsS --max-time 5 "$BASE/version" 2>/dev/null)" = v2 ]; then
			break
		fi
		i=$((i + 1))
		sleep 0.2
	done
	if [ "$(curl -fsS --max-time 5 "$BASE/version" 2>/dev/null)" = v2 ]; then
		echo "ok   script is reloaded after the file changes"
	else
		echo "FAIL script was not reloaded after the file changed"
		reload_bad=1
	fi
fi
if [ "$reload_bad" -eq 0 ]; then
	pass=$((pass + 1))
else
	fail=$((fail + 1))
fi

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
