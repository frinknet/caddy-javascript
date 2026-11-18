# caddy-javascript

[![ci](https://github.com/frinknet/caddy-javascript/actions/workflows/ci.yml/badge.svg)](https://github.com/frinknet/caddy-javascript/actions/workflows/ci.yml)

Run Express-style JavaScript inside [Caddy](https://caddyserver.com). The plugin
embeds [Sobek](https://github.com/grafana/sobek), a pure-Go ECMAScript engine,
and drives it from a single-threaded event loop, so scripts run in-process with
no CGO, no V8, and no external runtime.

```caddyfile
example.com {
	javascript /var/www/my-app/server.js
}
```

```js
app.get('/', (req, res) => {
	res.json({ ok: true, path: req.path })
})

app.post('/echo', (req, res) => {
	res.status(201).json(req.body)
})

app.use((req, res) => {
	res.status(404).send('not found')
})
```

## Contents

- [Features](#features)
- [Quick start](#quick-start)
- [Installation](#installation)
- [Configuration](#configuration)
- [JavaScript API](#javascript-api)
- [Request lifecycle](#request-lifecycle)
- [Status codes and errors](#status-codes-and-errors)
- [Security](#security)
- [Metrics and logging](#metrics-and-logging)
- [Operations and deployment](#operations-and-deployment)
- [Limitations and roadmap](#limitations-and-roadmap)
- [Development](#development)
- [Compatibility](#compatibility)
- [Layout](#layout)
- [Troubleshooting](#troubleshooting)
- [License](#license)

## Features

- Pure Go, `CGO_ENABLED=0`, cross-compiles with `xcaddy`.
- Express-shaped routing (`app.get`, `app.post`, `app.use`) with exact path
  matching.
- Request and response helpers: JSON and form bodies, query strings, headers,
  status codes, redirects, content types.
- Async completion: timers, outbound `fetch`, and async `fs` callbacks keep the
  request open until the response is written.
- Sandboxed filesystem rooted at a directory, using `os.Root` to block `..`
  and symlink escapes.
- Environment allowlist and an outbound HTTP allowlist with private-address
  blocking.
- Per-request execution watchdog that interrupts runaway scripts and recovers
  the runtime for the next request.
- Hot reload on file change with rollback to the last working script.
- Bounded queue with 503 backpressure and a graceful drain on shutdown.
- Structured logs and Prometheus metrics.

## Quick start

1. Build Caddy with the plugin (see [Installation](#installation)).
2. Save the script above as `/var/www/my-app/server.js`.
3. Point a site at it:

```caddyfile
:8080 {
	javascript /var/www/my-app/server.js
}
```

4. Run Caddy and try the routes:

```sh
caddy run --config Caddyfile --adapter caddyfile
curl -s localhost:8080/            # {"ok":true,"path":"/"}
curl -s -X POST localhost:8080/echo \
	-H 'Content-Type: application/json' -d '{"a":1}'
```

## Installation

### Build with xcaddy

The plugin is pure Go, so the standard Caddy build works:

```sh
xcaddy build --with github.com/frinknet/caddy-javascript
```

To build from a local checkout instead of the module proxy:

```sh
xcaddy build --with github.com/frinknet/caddy-javascript=/path/to/caddy-javascript
```

The resulting `caddy` binary has the `javascript` directive and the
`http.handlers.javascript` module registered.

### Build the container image

```sh
./build.sh
```

`build.sh` composes the module metadata from `NOTICE` and builds the image
tagged `caddy-javascript:test`. It also rewrites the repo slug in `NOTICE`,
`Dockerfile`, and `go.mod` from that metadata, so keep the file's first three
lines intact. The Dockerfile is multi-stage:

- build stage: `caddy:2.11.9-builder-alpine`, `xcaddy build`, then collects
  third-party license files and compresses the binary with `upx`.
- runtime stage: `FROM scratch` with the Caddy binary, toybox, CA
  certificates, tzdata, and licenses. `www/` is copied to `/srv/www`, and
  `www/Caddyfile` becomes `/etc/caddy/Caddyfile`.

The image entrypoint is `/bin/caddy`, default command
`run --config /etc/caddy/Caddyfile --adapter caddyfile`, and the data volume is
`/var/lib/caddy`.

## Configuration

### Caddyfile

```
javascript <path_to_server_script> {
	root        <dir>      # sandbox root for fs, defaults to the script's directory
	timeout     <duration> # per-request execution budget, default 3s
	queue_size  <n>        # event queue depth before 503s, default 5000
	max_body    <bytes>    # request body cap, default 1048576
	watch       [bool]     # reload on file change, default on
	fallthrough            # call the next handler when no route matches
	env_allow   <names...> # process.env allowlist; "*" exposes everything
	fetch_allow <hosts...> # fetch host allowlist
}
```

The directive takes exactly one argument, the script path. Unknown options are
an error. `env_allow` and `fetch_allow` may be repeated, and each occurrence
takes one or more names.

| Option | Default | Notes |
|---|---|---|
| script path | required | Resolved relative to the Caddy process working directory, then made absolute. |
| `root` | script's directory | Sandbox root for `fs` and `path`. Also made absolute. |
| `timeout` | `3s` | Go duration. Overrides the handler invocation budget, not async callbacks. `0` means the default. |
| `queue_size` | `5000` | Number of queued tasks (requests plus async callbacks). |
| `max_body` | `1048576` | Bytes. Larger bodies get `413`. `0` means the default. |
| `watch` | on | `watch`, `watch true`, `watch false`. Watches the script's directory. |
| `fallthrough` | off | When no route or fallback matches, call the next HTTP handler. |
| `env_allow` | empty | Each name is copied into `process.env`. `*` exposes the whole environment. |
| `fetch_allow` | empty | Exact host matches, case-insensitive. `*` allows any host that resolves only to public addresses. |

Defaults are applied twice: the Caddyfile parser fills them in, and
`Provision` applies them again for JSON configuration.

#### Site-wide use

```caddyfile
example.com {
	javascript /var/www/my-app/server.js
}
```

#### Scoped to a path

The directive can live inside `handle` blocks like any other HTTP handler:

```caddyfile
api.example.com {
	@api path /api/*
	handle @api {
		javascript /var/www/my-app/api-router.js
	}
	handle {
		respond "Not found" 404
	}
}
```

#### With static files

`javascript` is ordered before `file_server`, so `fallthrough` lets unmatched
requests reach the file server:

```caddyfile
example.com {
	root * /var/www/site
	javascript /var/www/site/app.js {
		fallthrough
	}
	file_server
}
```

#### Multiple apps

Each `javascript` instance owns its own event loop, router, and VM. Instances
in different sites or different `handle` blocks share nothing.

### JSON configuration

The Caddyfile adapts to a regular Caddy route handler. `caddy adapt
--config Caddyfile --pretty` shows the exact shape for your version:

```json
{
  "apps": {
    "http": {
      "servers": {
        "srv0": {
          "listen": [":8080"],
          "routes": [
            {
              "handle": [
                {
                  "handler": "javascript",
                  "script_path": "/srv/app/server.js",
                  "root": "/srv/app",
                  "timeout": "2s",
                  "queue_size": 2000,
                  "max_body": 2097152,
                  "watch": false,
                  "env_allow": ["API_KEY"],
                  "fetch_allow": ["api.example.com"]
                }
              ]
            }
          ]
        }
      }
    }
  }
}
```

`timeout` is a Caddy duration (`"2s"`, `"500ms"`). Omitted fields fall back to
the defaults listed above.

## JavaScript API

### Routing: `app`

| Call | Behavior |
|---|---|
| `app.get(path, fn)` | Register a GET handler for an exact path. |
| `app.post(path, fn)` | Register a POST handler for an exact path. |
| `app.use(fn)` | Register the fallback handler for unmatched requests. |

Details:

- Matching is an exact string compare on `method + " " + path`. No parameters,
  wildcards, trailing-slash normalization, or case folding.
- The query string is not part of the match.
- Registering the same method and path twice overwrites the earlier handler.
- `app.use` sets a single fallback; calling it again replaces the previous one.
  There is no `next()` and no middleware chain yet.
- Passing a non-function throws a `TypeError`.
- Only GET and POST exist. Other methods never match, so with `fallthrough` off
  they return 404.

### Request: `req`

| Property | Description |
|---|---|
| `req.path` | Decoded URL path. |
| `req.method` | HTTP method. |
| `req.host` | Request `Host`, including port when present. |
| `req.headers` | Object of header values, canonical names (`Content-Type`), first value only. |
| `req.header(name)` | Case-insensitive header lookup, first value or `undefined`. |
| `req.query` | Object of query parameters, first value per name. |
| `req.queryAll` | Object of arrays, every value per name. |
| `req.body` | Parsed body (see below). |

Body parsing is driven by `Content-Type`:

| Content type | `req.body` |
|---|---|
| `application/json` or any `+json` suffix | Parsed JSON value, or the raw string when parsing fails. |
| `application/x-www-form-urlencoded` | Object with the first value per field. |
| anything else, including multipart | Raw string. |
| empty body | `null`. |

Requests larger than `max_body` are rejected with `413` before the script runs.
Reading the body can fail earlier; that returns `400`.

### Response: `res`

| Call | Behavior |
|---|---|
| `res.status(code)` | Set the status code. Chainable. Applies only until the response is written. |
| `res.set(name, value)` | Set a header. Chainable. |
| `res.type(t)` | Set `Content-Type`. Chainable. |
| `res.send(data)` | Send a body. Strings are sent as `text/plain; charset=utf-8`; other values are JSON-encoded as `application/json; charset=utf-8`; `null`/`undefined` send an empty body. |
| `res.json(obj)` | JSON-encode and send with `application/json; charset=utf-8`. |
| `res.end([data])` | Send the optional body, or finish with the status already set. |
| `res.redirect([code,] url)` | Set `Location` and finish. Default status is `302`. |
| `res.flush()` | Flush the underlying `http.ResponseWriter` when it supports it. |

Rules and caveats:

- The first write commits the status and headers. Later `res.status` and
  `res.set` calls have no effect on what was already sent.
- Response helpers are chainable, so `res.status(201).type('text/plain').send('ok')`
  works.
- There is no streaming writer. `res.send` writes the whole body, and
  `res.flush()` only flushes what is already buffered.
- A handler that returns without writing anything yields `404`, even if it
  matched a route.

### Timers

`setTimeout(fn, ms)` schedules `fn` back on the event loop. The delay is
clamped at zero and returns `undefined`, so timers cannot be cancelled. Pending
timers keep the request open and are abandoned on shutdown. There is no
`setInterval` yet.

### `fetch`

`fetch(url, cb)` performs an asynchronous GET and delivers `cb(err, res)` on the
event loop. `err` is a string, `res.status` is the numeric status, and
`res.text` is the body.

```js
app.get('/proxy', (req, res) => {
	fetch('https://api.example.com/data', (err, upstream) => {
		if (err) return res.status(502).send(String(err))
		res.status(upstream.status).type('application/json').send(upstream.text)
	})
})
```

Behavior and limits:

- Requests are denied unless the host is in `fetch_allow`.
- A listed host is allowed as-is, including internal hostnames. With `*`, the
  host is resolved and every address must be public.
- Redirects are re-checked against the same rules, up to 5 hops.
- Request timeout is 10s, and the response body is capped at 1 MiB.
- Only GET is supported. There is no options object, no request body, no
  response headers, and no Promise form.
- Blocking is an error to the callback, so scripts decide the HTTP status.

### `fs`

| Call | Behavior |
|---|---|
| `fs.readFileSync(path)` | Return the file as a string, or throw. |
| `fs.writeFileSync(path, data)` | Write a string with mode `0644`, or throw. |
| `fs.readFile(path, cb)` | Call `cb(err, data)` on the event loop. `err` is a string or `null`. |
| `fs.writeFile(path, data, cb)` | Call `cb(err)` on the event loop. `err` is a string or `null`. |

Paths are confined to `root` using `os.Root`, which blocks `..` traversal and
symlink escapes. Both relative and absolute-looking names are resolved inside
the root: `/etc/passwd` reads `<root>/etc/passwd`. Missing directories are not
created, and there is no `unlink`, `readdir`, or stream API.

```js
const fs = require('fs')
app.get('/config', (req, res) => {
	fs.readFile('config.json', (err, data) => {
		if (err) return res.status(500).send(err)
		res.type('application/json').send(data)
	})
})
```

### `path`

`path.join(...parts)` joins with POSIX semantics, regardless of the host
filesystem. Use it to build names for `fs`.

### `require`

`require` supports only the built-in shims:

- `fs` and `node:fs`
- `path` and `node:path`

Anything else throws. There is no local module loading, no `node_modules`
resolution, and no package manager.

### `process.env`

The object is empty unless names are listed in `env_allow`. Values are captured
when the script loads or reloads, so changing the process environment at
runtime requires a reload. `env_allow *` exposes everything, which is rarely a
good idea for secrets.

## Request lifecycle

### Async completion

The HTTP handler does not return when the script returns. It waits until the
response is written or until no async work remains for that request:

1. Caddy reads and parses the body, then submits a task to the event loop.
2. The route handler runs on the loop goroutine.
3. If the handler schedules `fetch`, timers, or async `fs`, the exchange keeps
   a pending count. Callbacks are re-submitted to the same loop.
4. The request finishes when the response is written, or when the handler has
   returned and no callbacks are pending.
5. If the client disconnects, Caddy returns and the exchange is abandoned; the
   script may still finish the work.

This is what allows a response to be completed from a callback.

### Timeouts

`timeout` bounds the synchronous handler invocation. When it expires the VM is
interrupted, the interrupt flag is cleared, and the runtime stays usable for
later requests. If nothing was written the client gets `504`; otherwise the
partial response stands. Async callback execution is not covered by this
watchdog yet, so a slow callback can hold a request open.

### Backpressure

All requests and async callbacks share one bounded queue per directive
instance. When the queue is full:

- new requests get `503 Service Unavailable`.
- async callbacks are dropped and logged, which can leave a response
  unfinished.

Raise `queue_size` to absorb bursts, or make handlers cheaper. Requests are
serialized: JavaScript never runs concurrently within one instance, so a slow
handler delays the ones behind it.

### Reloading

With `watch` on, the plugin watches the script's directory and reloads when the
script is written, created, or renamed, debounced by 200ms.

- A reload builds a new VM and route table. In-flight exchanges keep using the
  program they started with.
- A reload that fails to parse or throws logs
  `javascript: reload failed, keeping previous script` and leaves the old
  program serving.
- A failure during startup is fatal: Caddy refuses to start with a script that
  throws while loading.
- Watching needs functional inotify. Some network filesystems do not deliver
  events; use `watch false` there.

## Status codes and errors

| Situation | Response |
|---|---|
| Route matched and wrote a response | Whatever the script sent. |
| No route, no fallback, `fallthrough` off | `404 Not Found` (plain text). |
| No route, no fallback, `fallthrough` on | Next HTTP handler. |
| Route matched, handler wrote nothing | `404 Not Found`. |
| Script throws, nothing written | `500 Internal Server Error`. |
| Handler exceeds `timeout`, nothing written | `504 Gateway Timeout`. |
| Handler exceeds `timeout` after writing | Partial response stands, error logged. |
| Body exceeds `max_body` | `413 Request Entity Too Large`. |
| Body read error | `400 Bad Request`. |
| Event queue full | `503 Service Unavailable`. |
| Runtime missing after a failed init | `500`, logged as `javascript: runtime unavailable`. |

Script errors are logged with method and path through Caddy's logger at error
level. Panics recovered by the event loop are logged as
`javascript: recovered from panic`.

## Security

Treat scripts as trusted server-side code. The sandbox is about blast radius,
not about running untrusted code.

- **Filesystem.** `fs` and `path` are rooted at `root`, defaulting to the
  script's directory. `os.Root` blocks `..` and symlink escapes. Everything
  under the root is readable and writable by the script.
- **Network.** `fetch` is denied by default. Listing a host allows it verbatim,
  which is the escape hatch for internal services. `*` still refuses private,
  loopback, link-local, multicast, and unspecified addresses, including after
  redirects.
- **Environment.** `process.env` is empty unless allowlisted. Do not combine
  `env_allow *` with untrusted scripts.
- **Bodies.** `max_body` caps request buffering. Response size is not capped.
- **Code execution.** There is no `eval` allowlist control: the whole point is
  to run the script. A bug or infinite loop in a handler is contained by the
  timeout and by process isolation only.

Deploy Caddy itself with the usual precautions: non-root user, read-only
filesystem where possible, and no admin API exposure. The bundled image runs as
root by default, like the upstream Caddy image.

## Metrics and logging

Caddy's Prometheus endpoint exposes:

| Metric | Type | Meaning |
|---|---|---|
| `caddy_javascript_requests_total{status}` | counter | Responses the handler completed, labelled by status. |
| `caddy_javascript_timeouts_total` | counter | Handler invocations interrupted by the watchdog. |
| `caddy_javascript_queue_full_total` | counter | Requests rejected because the event queue was full. |
| `caddy_javascript_reloads_total` | counter | Successful script reloads. |

Log messages to look for:

- `javascript: reloaded script`
- `javascript: reload failed, keeping previous script`
- `javascript: handler error`
- `javascript: recovered from panic`
- `javascript: file watching disabled`
- `javascript: event queue full, dropping async callback`

## Operations and deployment

### Container

The scratch image has no shell utilities beyond toybox, config at
`/etc/caddy/Caddyfile`, static root at `/srv/www`, and data at `/var/lib/caddy`.
Mount your app and config:

```sh
docker run --rm -p 8080:80 \
	-v "$PWD/Caddyfile:/etc/caddy/Caddyfile:ro" \
	-v "$PWD/app:/srv/app:ro" \
	caddy-javascript:test
```

The health check polls Caddy's admin API on `127.0.0.1:2019`. The admin API is
not exposed outside the container by default.

### Sizing

- One event loop per directive instance. Multiple instances, sites, or Caddy
  processes are independent and share no VM state.
- `queue_size` is task slots, not concurrent requests. Memory per queued task is
  small, but dropped callbacks stall responses, so leave headroom.
- Reload rebuilds the VM, so routes are re-registered from the script on every
  reload.

## Limitations and roadmap

Not implemented today:

- `app.put`, `app.delete`, `app.all`, and other methods.
- Path parameters, wildcards, and `req.params`.
- Middleware chains and `next()`.
- `require('express')` and npm packages.
- Multipart bodies.
- Local module loading via `require`.
- `setInterval`, `clearTimeout`, and timer ids.
- `fetch` options, request bodies, response headers, and Promise form.
- Streaming responses, SSE, WebSocket upgrades, and `sendFile`.
- Request-level cancellation of async work.

`spec.md` describes the design target and the intended Express parity.

## Development

All Go tooling runs in Docker; the host needs no toolchain. The `caddy-sf-mod`
volume holds the Go module and build caches.

Format check, build, and vet:

```sh
docker run --rm \
  -v caddy-sf-mod:/mod \
  -v "$PWD":/src -w /src \
  -e GOMODCACHE=/mod -e GOCACHE=/mod/gocache -e CGO_ENABLED=0 \
  caddy:2.11.9-builder-alpine \
  sh -c 'gofmt -l . && go build ./... && go vet ./...'
```

Unit tests:

```sh
docker run --rm \
  -v caddy-sf-mod:/mod \
  -v "$PWD":/src -w /src \
  -e GOMODCACHE=/mod -e GOCACHE=/mod/gocache -e CGO_ENABLED=0 \
  caddy:2.11.9-builder-alpine \
  go test -count=1 ./...
```

Race detector (needs cgo, so install a compiler in the container):

```sh
docker run --rm \
  -v caddy-sf-mod:/mod \
  -v "$PWD":/src -w /src \
  -e GOMODCACHE=/mod -e GOCACHE=/mod/gocache -e CGO_ENABLED=1 \
  caddy:2.11.9-builder-alpine \
  sh -c 'apk add --no-cache gcc musl-dev && go test -race -count=1 ./...'
```

End-to-end suite. It builds the plugin image, starts it, and drives it over
HTTP with table-driven cases from `tests/e2e/cases`:

```sh
tests/e2e/run.sh          # rebuilds the image first
BUILD=0 tests/e2e/run.sh  # reuse the existing image
```

The harness copies `tests/e2e/app` to a temp directory and mounts it at
`/e2e/app` in the server and `/work` in the client, so reload tests can edit the
script. Without `CADDY_E2E_ADDR` the Go tests skip, which keeps a bare
`go test ./...` green.

Manual testing uses the sample app in `www/test`:

```sh
cd www/test
./run.sh    # caddy run --config Caddyfile --adapter caddyfile
```

CI runs all of the above: format, build, vet, unit tests, the race detector,
and the e2e suite. See `.github/workflows/ci.yml`.

When adding request/response checks, add a case file under `tests/e2e/cases`
instead of new Go code. The runner understands `method`, `path`, `body`,
`headers`, `wantStatus`, `wantContentType`, `wantBodyEquals`,
`wantBodyContains`, and `wantHeaders`.

## Compatibility

| Component | Version |
|---|---|
| Caddy | v2.11.9 (see `go.mod`) |
| Go | 1.26 |
| Sobek | pinned in `go.mod` |
| CGO | not required |

The plugin registers `http.handlers.javascript` and places the `javascript`
directive before `file_server`.

## Layout

```
javascript.go        Caddy module glue: registration, Provision, Cleanup, ServeHTTP
metrics.go           Prometheus counters
pkg/config           options, defaults, validation, Caddyfile parsing
pkg/sched            task queue, single-thread run loop, backpressure, drain
pkg/bridge           Sobek runtime, JS globals, req/res, timers, fetch, watchdog
pkg/router           route table and dispatch
pkg/files            sandboxed fs, path.join, fsnotify hot reload
www/test             sample Caddyfile, run.sh, and app for manual testing
tests/e2e            end-to-end harness, app, and case files
spec.md              design target
```

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| Caddy will not start, `failed to initialize runtime` | The script throws while loading, or the path is wrong. Check the path and run it under `watch false` with logs at debug level. |
| `404` on a route you registered | Path mismatch (trailing slash, case), wrong method, or the handler returned without writing. Matching is exact. |
| `404` for everything, no script errors | Script failed to load and the handler has no program, or the directive is in a `handle` block whose matcher does not match. |
| `503 Service Unavailable` | Queue full. Raise `queue_size`, or find the slow handler blocking the loop. |
| `504 Gateway Timeout` | `timeout` is too small, or the handler has an infinite loop. Async callbacks are not covered by the watchdog. |
| `413` | Body exceeds `max_body`. Raise it or reject large uploads in Caddy. |
| `fetch blocked: ...` | Host missing from `fetch_allow`, or `*` with a private resolution. Listed hosts are allowed verbatim. |
| `fs` `ENOENT` | Wrong `root`. It defaults to the script's directory, not the Caddy working directory. |
| Reload does nothing | `watch false`, no inotify from the filesystem, or the file was replaced without a matching event. Reload manually with `caddy reload`. |
| Unknown option | Typo in the block. Unknown keys are a parse error, not ignored. |
| `javascript` is not recognized | The binary was built without the plugin. Rebuild with `xcaddy --with github.com/frinknet/caddy-javascript`. |

## License

Apache-2.0. See `LICENSE` and `NOTICE`.