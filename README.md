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
- Express-shaped routing: `app.get`/`app.post`/`app.put`/`app.delete`/`app.all`,
  `app.use` middleware, ordered `next()` chains, path parameters, optional
  segments, wildcards, mounted routers, and static file middleware.
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

- build stage: `caddy:2.11.9-builder-alpine`, `xcaddy build`, then the caddy
  and plugin license files.
- runtime stage: `FROM scratch` with the Caddy binary, toybox, CA
  certificates, tzdata, and licenses. `www/` is copied to `/srv/www`, and
  `www/Caddyfile` becomes `/etc/caddy/Caddyfile`.

The image entrypoint is `/bin/caddy` and the default command is `run`, which
starts Caddy with an empty config: Caddy looks for `--config` in the working
directory, and the bundled config is at `/etc/caddy/Caddyfile`. Pass
`run --config /etc/caddy/Caddyfile --adapter caddyfile` to load it. Caddy keeps
its state in `/var/lib/caddy`; no volume is declared for it.

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
| `fallthrough` | off | When nothing matches, call the next HTTP handler. |
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
| `app.get(path, ...fns)` | Register a GET route. |
| `app.post(path, ...fns)` | Register a POST route. |
| `app.put`, `app.delete`, `app.patch`, `app.head`, `app.options` | Same, for those methods. |
| `app.all(path, ...fns)` | Register a route for every method. |
| `app.use([path,] ...fns)` | Register middleware. Without a path it matches every request; with one it matches that path and everything under it. |
| `app.listen(...)` | No-op that returns the app. Caddy owns the listener. |

`require('express')` returns a factory that hands back the same app object, so
unmodified Express style works:

```js
const express = require('express')
const app = express()

app.get('/', (req, res) => res.send('hi'))
app.listen(3000)
```

Layers run in registration order, the way Express runs its stack. A handler
advances the chain with `next()`, and `next(err)` skips ahead to the next error
handler, which is a function taking four arguments `(err, req, res, next)`. A
handler that never calls `next()` ends the chain. Writing a response does not
stop the chain, but the first write commits the response and later writes are
ignored. Registering the same method and path twice appends both, so both run.

Paths:

- `:name` captures one segment into `req.params.name`. `:name?` also matches
  when the segment is absent, and `*` captures the rest of the path into
  `req.params[0]`.
- Matching is case-insensitive and ignores a trailing slash, so `/Users/` and
  `/users` are the same route. The query string is not part of the match.
- `app.use` paths are prefixes, and the prefix is not stripped from `req.path`.
- Passing something that is not a function throws a `TypeError`. Calling a
  method the app does not implement throws too, because the property does not
  exist.

An error that no error handler takes is reported as `500 Internal Server
Error`, unless a response was already written.

### Routers: `express.Router()`

```js
const express = require('express')
const router = express.Router()

router.get('/users/:id', (req, res) => res.json({ id: req.params.id }))
router.use((req, res, next) => { res.set('X-API', '1'); next() })

app.use('/api', router) // GET /api/users/7 -> {"id":"7"}
```

`express.Router()` returns a router with the same route and `use` methods as
the app. Mount it with `app.use([path,] router)`, and mount routers inside
routers the same way. A mounted router's paths are relative to the mount path:
the mount prefix is removed before matching, so `router.get('/users')` mounted
at `/api` matches `/api/users`. Parameters captured by the mount path are
merged into `req.params`, and routes added to a router after it is mounted are
still seen.

`req.path` is not rewritten: handlers inside a router see the full request
path, matching the `app.use` rule above. A router that matches nothing falls
through to the next layer of its parent, so later middleware still runs. There
is no `router.route()` chain and no `router.param()`.

### Static files: `express.static()`

```js
const express = require('express')

app.use(express.static('public'))
app.use('/static', express.static('public'))
```

`express.static(root, [options])` returns middleware that serves files below
`root`, resolved inside the sandbox. Mounted at a path, the mount prefix is
stripped before the file is looked up. Directory requests serve the index file.
Supported options:

| Option | Default | Behavior |
|---|---|---|
| `index` | `"index.html"` | Index file for a directory. `false` disables it. |
| `fallthrough` | `true` | Continue the chain when the file is missing. `false` sends `404`. |
| `dotfiles` | `"ignore"` | `"allow"` serves dotted paths, `"deny"` sends `403`, `"ignore"` falls through. |

Only `GET` and `HEAD` are served; other methods fall through. Files are read
whole and served with `Content-Type`, `Content-Length`, and `Last-Modified`.
There is no range support, no conditional `304` handling, and no directory
redirect, so `/dir` and `/dir/` both serve the index. Dotfiles are ignored by
default.

### Request: `req`

| Property | Description |
|---|---|
| `req.path` | Decoded URL path. |
| `req.method` | HTTP method. |
| `req.host` | Request `Host`, including port when present. |
| `req.params` | Path parameters captured by the matched route. |
| `req.headers` | Object of header values, canonical names (`Content-Type`), first value only. |
| `req.header(name)` | Case-insensitive header lookup, first value or `undefined`. |
| `req.query` | Object of query parameters, first value per name. |
| `req.queryAll` | Object of arrays, every value per name. |
| `req.body` | Parsed body (see below). |

Body parsing is driven by `Content-Type`:

| Content type | `req.body` |
|---|---|
| `application/json` or any `+json` suffix | Parsed JSON value, or the raw string when parsing fails. |
| `application/x-www-form-urlencoded` | Object with one string per field, or an array when a field repeats. |
| `multipart/form-data` | Object with the first text value per field; file parts are ignored. |
| anything else | Raw string. |
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

`setTimeout(fn, ms)` and `setInterval(fn, ms)` schedule `fn` back on the
event loop and return a numeric id. Delays are clamped at zero, and
`setInterval` uses a minimum of 1ms. `clearTimeout(id)` and `clearInterval(id)`
cancel either kind of timer; missing or unknown ids are ignored.

Pending timers keep the request open, and an uncleared `setInterval` keeps it
open indefinitely. Outstanding timers are abandoned on shutdown.

### `fetch`

`fetch(url[, options])` returns a Promise. Passing a callback instead delivers
`cb(err, res)` on the event loop and keeps the older response shape.

```js
// Promise form
app.get('/proxy', (req, res) => {
	fetch('https://api.example.com/data')
		.then((upstream) => upstream.text())
		.then((text) => res.type('application/json').send(text))
		.catch((err) => res.status(502).send(err.message))
})

// Callback form; err is a string and res.text is the whole body
app.get('/proxy-cb', (req, res) => {
	fetch('https://api.example.com/data', (err, upstream) => {
		if (err) return res.status(502).send(String(err))
		res.status(upstream.status).type('application/json').send(upstream.text)
	})
})
```

Options:

| Option | Behavior |
|---|---|
| `method` | HTTP method, default `GET`. A body with `GET` or `HEAD` rejects. |
| `headers` | Plain object. Array values add repeated headers; `Host` sets the request host. |
| `body` | Request body as a string. |

The Promise resolves to a response with `status`, `statusText`, `ok`, `url`,
`redirected`, and `headers`. `headers` is an object of lowercased names plus
`get(name)`, `has(name)`, and `forEach(fn)`. The body is consumed with
`text()`, `json()`, or `body.getReader()`:

```js
app.get('/stream', (req, res) => {
	fetch('https://api.example.com/feed')
		.then((upstream) => {
			const reader = upstream.body.getReader()
			let out = ''
			const pump = () => reader.read().then((step) => {
				if (step.done) return res.send(out)
				out += step.value
				return pump()
			})
			return pump()
		})
		.catch((err) => res.status(502).send(err.message))
})
```

`reader.read()` resolves to `{ done, value }`, where `value` is a string chunk,
and `reader.cancel()` stops the body. A body can be consumed once, and the
reader and `text()`/`json()` are mutually exclusive. Only one read is
outstanding at a time, so a slow script backpressures the network.

The callback form exposes `status`, `statusText`, `ok`, `url`, `redirected`,
`headers`, and `text` (the whole body as a string). It has no streaming and no
`text()`/`json()` methods.

Behavior and limits:

- Requests are denied unless the host is in `fetch_allow`.
- A listed host is allowed as-is, including internal hostnames. With `*`, the
  host is resolved and every address must be public.
- Redirects are re-checked against the same rules, up to 5 hops.
- The timeout is 10s for the whole exchange, including streaming, and the
  response body is capped at 1 MiB.
- The Promise rejects with an `Error`; the callback's `err` remains a string.
- Request bodies are strings, and there is no `FormData`, `Blob`,
  `URLSearchParams`, `AbortSignal`, `arrayBuffer()`, or `clone()`.
- A Promise response body that is never consumed is abandoned; its request is
  closed when the timeout fires.

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

`require` resolves a CommonJS module and returns its `exports`. The built-in
shims always win:

- `fs` and `node:fs`
- `path` and `node:path`
- `express`

Anything else is treated as a path. Local specifiers (`./`, `../`, and `/`) are
resolved from the directory of the file doing the requiring. A bare name like
`leftpad` searches `node_modules` starting in that directory and walking up to
the root. Resolution tries the exact file, then `.js`, then `.json`, then a
directory's `package.json` `main`, then its `index.js` or `index.json`.

Each module runs once in its own scope with `module`, `exports`, `require`,
`__filename`, and `__dirname`, and its `exports` is cached for the life of the
runtime. A reload rebuilds the VM and so empties the cache; editing a module
does not reload by itself, only the entry script is watched.

Everything stays inside `root`. A module cannot reach above it, and there is no
package manager or npm install. The `express` shim provides the app factory
plus `Router()` and `static()`.

```js
// lib/greet.js
module.exports = (name) => `hello ${name}`

// server.js
const greet = require('./lib/greet')
const leftpad = require('leftpad') // from node_modules
app.get('/hi', (req, res) => res.send(greet(leftpad('x'))))
```

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
2. The layers that match the request run on the loop goroutine, in
   registration order, each advancing the chain with `next()`.
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

### Shutdown

When Caddy stops or reloads its configuration, the instance is cleaned up and
its event loop stops. The loop first drains what is already queued, running
those tasks in order for up to 5 seconds, then returns. Nothing new is accepted
once it starts stopping:

- New requests get `503 Service Unavailable`, because the loop no longer takes
  submissions.
- Async callbacks that land during the drain, from a pending `fetch`, `fs`, or
  timer, are dropped and logged as
  `javascript: event queue full, dropping async callback`. A response waiting on
  one of them is left unfinished; Caddy is shutting down anyway.
- Work still in flight outside the loop, such as an `os.Root` read or an
  outbound `fetch`, is abandoned when the drain deadline passes. Its callback
  arrives after the loop has stopped, so it is dropped.

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
| No route or middleware matched, `fallthrough` off | `404 Not Found` (plain text). |
| No route or middleware matched, `fallthrough` on | Next HTTP handler. |
| Route matched, handler wrote nothing | `404 Not Found`. |
| Script throws or calls `next(err)`, nothing written | `500 Internal Server Error`. |
| `next(err)` reaches an error handler | Whatever the error handler sent. |
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

- **Filesystem.** `fs`, `path`, and `express.static` are rooted at `root`,
  defaulting to the script's directory. `os.Root` blocks `..` and symlink
  escapes. Everything under the root is readable and writable by the script.
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
The default command is a bare `run`, so name the config on the command line.
Mount your app and config:

```sh
docker run --rm -p 8080:80 \
	-v "$PWD/Caddyfile:/etc/caddy/Caddyfile:ro" \
	-v "$PWD/app:/srv/app:ro" \
	caddy-javascript:test \
	run --config /etc/caddy/Caddyfile --adapter caddyfile
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

- npm installation.
- Server-side streaming responses, SSE, WebSocket upgrades, and `sendFile`.
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
HTTP with assertions in `tests/e2e/cases.sh`:

```sh
tests/e2e/run.sh          # rebuilds the image first
BUILD=0 tests/e2e/run.sh  # reuse the existing image
```

`run.sh` is the container orchestration: it copies `tests/e2e/app` to a temp
directory and mounts it at `/e2e/app` in the server, then runs `cases.sh` in a
client container that sees the same app at `/work` so the reload check can edit
the script. The checks are plain shell and curl, and the harness runs entirely
in Docker, so a bare `go test ./...` stays green.

Manual testing uses the sample app in `www/test`:

```sh
cd www/test
./run.sh    # caddy run --config Caddyfile --adapter caddyfile
```

CI runs all of the above: format, build, vet, unit tests, the race detector,
and the e2e suite. See `.github/workflows/ci.yml`.

When adding request/response checks, add a `check` line to
`tests/e2e/cases.sh` instead of new Go code. The helper takes a name, method,
path, and any of `--status`, `--ctype`, `--equals`, `--contains`, `--header`,
`--hdr`, `--data`, and `--data-file`.

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
pkg/config           options, defaults, validation, Caddyfile parsing
pkg/metric           Prometheus counters
pkg/sched            task queue, single-thread run loop, backpressure, drain
pkg/bridge           Sobek runtime, JS globals, req/res, timers, fetch, watchdog
pkg/router           route table and dispatch
pkg/files            sandboxed fs, path.join, fsnotify hot reload
www/test             sample Caddyfile, run.sh, and app for manual testing
tests/e2e            end-to-end bash harness, server app, and check script
spec.md              design target
```

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| Caddy will not start, `failed to initialize runtime` | The script throws while loading, or the path is wrong. Check the path and run it under `watch false` with logs at debug level. |
| `404` on a route you registered | Wrong path or method, or a handler returned without writing. Trailing slashes and case are ignored, so check for a typo or a missing `next()`. |
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