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
- [Admin eval](#admin-eval)
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
  `app.use` middleware, ordered `next()` chains, `next('route')`,
  `next('router')`, path parameters, optional segments, wildcards, mounted
  routers, and static file middleware.
- Express-compatible body parsing: `express.json()`, `express.urlencoded()`,
  `express.text()`, `express.raw()`, and `require('body-parser')` with the
  `limit`, `type`, `strict`, and `verify` options, plus application settings and
  `app.locals`.
- Request and response helpers: JSON and form bodies, query strings, cookies,
  headers, status codes, redirects, content types, `res.download`,
  `res.render` with registered or required view engines, `res.format`, and
  `res.vary`.
- Async route handling: handlers may be `async` functions and promises returned
  by a handler keep the request open.
- Module support: CommonJS `require` with `node_modules` resolution, ES modules
  (`import`/`export`), and pure-Go shims for `fs`, `path`, `express`, `buffer`,
  `events`, `url`, `querystring`, and `util`.
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
	async_budget <n>       # max outstanding async callbacks per request, default 64
	max_body    <bytes>    # request body cap, default 1048576
	watch       [bool]     # reload on file change, default on
	fallthrough            # call the next handler when no route matches
	eval                   # expose the admin eval endpoint for this directive
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
| `timeout` | `3s` | Go duration. Budget for each handler invocation and each async callback. `0` means the default. |
| `queue_size` | `5000` | Number of queued tasks per event-loop queue (requests, async callbacks, and background jobs). |
| `async_budget` | `64` | Maximum outstanding async callbacks per request. Callbacks over the budget are rejected. |
| `max_body` | `1048576` | Bytes. Larger bodies get `413`. `0` means the default. |
| `watch` | on | `watch`, `watch true`, `watch false`. Watches the script's directory. |
| `fallthrough` | off | When nothing matches, call the next HTTP handler. |
| `eval` | off | Expose the admin eval endpoint for this directive. See [Admin eval](#admin-eval). |
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
                  "async_budget": 32,
                  "max_body": 2097152,
                  "watch": false,
                  "eval": true,
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
| `app.use([path,] ...fns)` | Register middleware. Without a path it matches every request; with one it matches that path and everything under it. Arrays of middleware are accepted. |
| `app.set(name, value)` | Store an application setting. |
| `app.get(name)` | Read a setting when called with one argument. |
| `app.enable(name)` / `app.disable(name)` | Set a boolean setting. |
| `app.enabled(name)` / `app.disabled(name)` | Read a boolean setting. |
| `app.engine(ext, fn)` | Register a view engine for an extension such as `'html'` or `'.html'`. |
| `app.render(name[, options], cb)` | Render a view and call `cb(err, html)`. |
| `app.locals` | Application-local values shared with every request. |
| `app.listen(...)` | No-op that returns the app. Caddy owns the listener. |

`require('express')` returns a factory that hands back the same app object, so
unmodified Express style works:

```js
const express = require('express')
const app = express()

app.use(express.json())
app.get('/', (req, res) => res.send('hi'))
app.listen(3000)
```

`express.json()`, `express.urlencoded()`, `express.text()`, `express.raw()`,
and `require('body-parser')` return middleware. Bodies are already parsed from
`Content-Type` before the handler runs; these middleware re-parse the body under
their own rules when the content type matches. Each accepts the body-parser
options:

| Option | Behavior |
|---|---|
| `limit` | Byte count or size string (`'100kb'`, `'1mb'`, `512`). Defaults to `100kb`. A larger body fails with `413` and `err.type === 'entity.too.large'`. |
| `type` | Content type to accept: a string with `*` wildcards (`'application/*'`, `'*/json'`), a bare subtype (`'json'`), a function `(req) => boolean`, or an array of these. An unmatched request runs the next layer with `req.body` untouched. Defaults to `application/json`, `application/x-www-form-urlencoded`, `text/plain`, and `application/octet-stream` respectively. |
| `strict` | `express.json` only, default `true`. Accepts only objects and arrays; a scalar fails with `400` and `err.type === 'entity.parse.failed'`. |
| `verify` | `(req, res, buf)` called with the raw body as a `Buffer` before parsing. Throwing fails the request with the error's `status` when set, otherwise `403`. |

A failed parse reaches `next(err)` with `err.status`/`err.statusCode` and
`err.type`, so an error handler can answer it. Without one, the response uses
that status, the way Express's final handler does. An empty body becomes `{}`
for `json`/`urlencoded` and `''` for `text`/`raw`.

Layers run in registration order, the way Express runs its stack. A handler
advances the chain with `next()`, and `next(err)` skips ahead to the next error
handler, which is a function taking four arguments `(err, req, res, next)`.
`next('route')` skips the rest of the current route's handlers. `next('router')`
leaves the current router, including routers nested inside it, and resumes at
the parent router's next layer. A handler that
never calls `next()` ends the chain. Writing a response does not stop the
chain, but the first write commits the response and later writes are ignored.
Registering the same method and path twice appends both, so both run. A
handler may be an `async` function, and a promise returned by a handler keeps
the request open until it settles.

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
Error`, unless it carries an HTTP status in `err.status` or `err.statusCode`, as
the body parser errors do; then that status answers the request.

### Views: `res.render()`

```js
const fs = require('fs')

app.engine('html', (file, options, cb) => {
  fs.readFile(file, (err, text) => {
    if (err) return cb(err)
    cb(null, text.replace('{{title}}', options.title))
  })
})
app.set('views', 'views')
app.set('view engine', 'html')

app.get('/', (req, res) => {
  res.render('index', { title: 'Home' })
})
```

`res.render(view[, options][, cb])` resolves the view below the `views`
setting (`"views"` in the sandbox root by default; a directory string or an
array of them, looked up in order), appends the `view engine` extension when
the name has none, and calls its engine with `(file, options, cb)`. Options
merge `app.locals`, then `res.locals`, then the caller's object;
`options.settings` exposes the app settings and `options._locals` the response
locals.

Without a callback the rendered HTML is sent as `text/html` and a rendered
error goes to `next(err)`. With a callback the HTML is only passed to
`cb(err, html)`, so the handler decides what to send. `app.render` has the same
engine and lookup rules but requires the callback and has no response locals.

Engines can also be resolved with `require` from the extension, so a vendored
engine module exporting `__express`, `renderFile`, or a function works without
`app.engine` registration. Engines may finish asynchronously.

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
through to the next layer of its parent, so later middleware still runs.

`router.route(path)` returns a route object whose method handlers (`get`,
`post`, and so on) share the path and chain:

```js
router.route('/users/:id')
	.get((req, res) => res.json({ id: req.params.id }))
	.post((req, res) => res.status(201).end())
```

`router.param(name, fn)` registers a callback for a captured parameter. It runs
once per request before the first matching route handler, with
`fn(req, res, next, value, name)`, and `next()` continues. Param callbacks are
local to the router that registered them.

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
| `maxAge` | unset | Sets `Cache-Control: public, max-age=N` from milliseconds. |
| `etag` | `true` | Generates an `ETag` and honors `If-None-Match`. |
| `extensions` | none | Extensions tried in order when the file is missing, e.g. `['html']`. |

Only `GET` and `HEAD` are served; other methods fall through. Files are
streamed with `Content-Type`, `Content-Length`, `Last-Modified`, and an `ETag`.
`Range` requests get `206` with `Content-Range`, and `If-None-Match` or
`If-Modified-Since` can produce `304`. A directory requested without a trailing
slash redirects to the slash with `301`. Dotfiles are ignored by default.

### Request: `req`

| Property | Description |
|---|---|
| `req.path` | Decoded URL path. |
| `req.method` | HTTP method. |
| `req.host` | Request `Host`, including port when present. |
| `req.hostname` | Request host without the port. |
| `req.baseUrl` | Mount path consumed by enclosing routers, such as `/api/acme`; empty at the app level. |
| `req.url` | Request path including the query string. |
| `req.originalUrl` | Request path as received, including the query string. Equal to `req.url` because handlers never rewrite the path. |
| `req.ip` | Client address from the connection, without the port. |
| `req.protocol` | `"https"` when the connection uses TLS, otherwise `"http"`. |
| `req.secure` | `true` when `req.protocol` is `"https"`. |
| `req.xhr` | `true` when `X-Requested-With` is `XMLHttpRequest`. |
| `req.app` | The application object. |
| `req.route` | The last matched route: `path` relative to its router and `method`. Set only by routes, not middleware. |
| `req.params` | Path parameters captured by the matched route. |
| `req.cookies` | Cookies parsed from the `Cookie` header. |
| `req.headers` | Object of header values, canonical names (`Content-Type`), first value only. |
| `req.header(name)` | Case-insensitive header lookup, first value or `undefined`. |
| `req.get(name)` | Alias of `req.header(name)`. |
| `req.param(name[, default])` | Look up a route parameter, body field, then query value. |
| `req.accepts(...types)` | Best matching `Accept` type, or `false`. |
| `req.is(...types)` | Matched `Content-Type`, or `false`. |
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
Reading the body can fail earlier; that returns `400`. The parser middleware
above add their own, smaller checks; see their options.

### Response: `res`

| Call | Behavior |
|---|---|
| `res.status(code)` | Set the status code. Chainable. Applies only until the response is written. |
| `res.set(name, value)` | Set a header. Also accepts an object of names to values. Chainable. |
| `res.header(name, value)` | Alias of `res.set`. |
| `res.get(name)` / `res.getHeader(name)` | Read a response header. |
| `res.removeHeader(name)` | Remove a response header. Chainable. |
| `res.append(name, value)` | Append a header value without replacing existing ones; an array appends each item. Chainable. |
| `res.type(t)` | Set `Content-Type`. Extensions such as `json` and `html` are resolved through the MIME table. Chainable. |
| `res.locals` | Per-response values for templates and handlers. |
| `res.app` | The application object. |
| `res.send(data)` | Send a body. Strings are sent as `text/plain; charset=utf-8`; Buffers and ArrayBuffers as `application/octet-stream`; other values are JSON-encoded as `application/json; charset=utf-8`; `null`/`undefined` send an empty body. |
| `res.json(obj)` | JSON-encode and send with `application/json; charset=utf-8`. |
| `res.sendStatus(code)` | Set the status, send the standard status text as `text/plain`, and finish. `204` and `304` send no body. |
| `res.write(data)` | Write a chunk and keep the response open. |
| `res.end([data])` | Send the optional body, or finish with the status already set. |
| `res.redirect([code,] url)` | Set `Location` and finish. Default status is `302`; `'back'` uses the `Referer` header. |
| `res.location(url)` | Set `Location` without finishing. `'back'` uses the `Referer` header, falling back to `/`. Chainable. |
| `res.cookie(name, value[, opts])` | Add a `Set-Cookie` header. Options: `path` (default `/`), `domain`, `maxAge` (milliseconds), `expires`, `secure`, `httpOnly`, `sameSite`, and `encode` (a function; defaults to `encodeURIComponent`). Chainable. |
| `res.clearCookie(name[, opts])` | Add an expired `Set-Cookie` header. Chainable. |
| `res.attachment([filename])` | Set `Content-Disposition: attachment`. Chainable. |
| `res.download(name[, filename][, opts][, cb])` | Serve a file as an attachment. |
| `res.format(obj)` | Negotiate the `Accept` header and invoke the matching callback with `(req, res, next)`. |
| `res.vary(field)` | Add a `Vary` header. Chainable. |
| `res.render(view[, options][, cb])` | Render a view with the registered engine. Without `cb`, send the result as `text/html`. |
| `res.sendFile(name[, opts][, cb])` | Serve a file from the sandbox. Options: `root` (paths are confined to it; a bare string is accepted as the root), `headers` (extra response headers), `maxAge` (milliseconds, sets `Cache-Control`), and `dotfiles` (`ignore` by default, or `deny`). A missing or refused file answers `404`, or calls `cb(err)` when given. Reads are asynchronous and bodies are skipped for `HEAD`. |
| `res.flush()` | Flush the underlying `http.ResponseWriter` when it supports it. |

Rules and caveats:

- The first write commits the status and headers. Later `res.status` and
  `res.set` calls have no effect on what was already sent.
- Response helpers are chainable, so `res.status(201).type('text/plain').send('ok')`
  works.
- The response is not a streaming API. `res.write` writes a chunk, but there
  is no backpressure, and `res.flush()` only flushes what is already buffered.
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
| `body` | String, `Blob`, `FormData`, `URLSearchParams`, or `ArrayBuffer`/typed array. |
| `redirect` | `follow` (default), `error`, or `manual`. |
| `cache` | `default`, `no-store`, `reload`, `no-cache`, `force-cache`, or `only-if-cached`. |
| `credentials` | `same-origin` (default), `omit`, or `include`. |
| `signal` | An `AbortSignal` from `new AbortController()`. |

The Promise resolves to a response with `status`, `statusText`, `ok`, `url`,
`redirected`, and `headers`. `headers` is an object of lowercased names plus
`get(name)`, `has(name)`, and `forEach(fn)`. The body is consumed with
`text()`, `json()`, `arrayBuffer()`, or `body.getReader()`. `clone()` returns
an independent response backed by a buffered copy of the body:

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
- The Promise rejects with an `Error`; the callback's `err` is also an
  `Error`, with `message` and a `code` such as `ERR_FETCH_FAILED`, `ABORT_ERR`,
  or `ETIMEDOUT`.
- `AbortController` and `AbortSignal` cancel an in-flight request. The signal
  rejects the Promise or calls the callback with an abort error.
- `redirect` `error` rejects on a redirect; `manual` resolves with the 3xx
  response. `cache` maps to `Cache-Control` headers and `credentials` `omit`
  drops the `Cookie` header. There is no HTTP cache or cookie jar.
- `clone()` buffers the body, so a cloned response cannot be streamed.
- A Promise response body that is never consumed is abandoned; its request is
  closed when the timeout fires.

### `fs`

| Call | Behavior |
|---|---|
| `fs.readFileSync(path)` | Return the file as a string, or throw. |
| `fs.writeFileSync(path, data)` | Write a string with mode `0644`, or throw. |
| `fs.readFile(path, cb)` | Call `cb(err, data)` on the event loop. `err` is an `Error` with `message` and a `code` such as `ENOENT`, or `null`. |
| `fs.writeFile(path, data, cb)` | Call `cb(err)` on the event loop. `err` is an `Error` with `message` and a `code`, or `null`. |

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
- `buffer` and `node:buffer`
- `events` and `node:events`
- `url` and `node:url`
- `querystring` and `node:querystring`
- `util` and `node:util`

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
plus `Router()` and `static()`. The `buffer` shim also installs a global
`Buffer`.

### ES modules

An entry script or module that uses top-level `import`/`export` is evaluated as
an ES module. Relative specifiers resolve against the importing file and bare
specifiers walk `node_modules` exactly like `require`. CommonJS modules and the
built-in shims are adapted automatically, so `import path from "path"`,
`import { Buffer } from "buffer"`, and `import { Router } from "express"` all
work. JSON imports yield the parsed value. Top-level `await` is not supported:
the module must evaluate synchronously.

```js
// server.mjs
import { Router } from "express";
import { Buffer } from "buffer";

const router = Router();
router.get("/bin", (req, res) => {
	res.send(Buffer.from("ok").toString());
});
```

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
   a pending count. Callbacks are re-submitted to the same loop, and each one
   runs under the `timeout` watchdog.
4. The request finishes when the response is written, or when the handler has
   returned and no callbacks are pending.
5. If the client disconnects, Caddy returns and the exchange is abandoned; the
   script may still finish the work.

This is what allows a response to be completed from a callback.

### Timeouts

`timeout` bounds every JavaScript invocation: the synchronous handler call and
each async callback (`setTimeout`, `setInterval`, `fetch`, `fs`). When a budget
expires the VM is interrupted, the interrupt flag is cleared, and the runtime
stays usable for later requests. If nothing was written the client gets `504`;
otherwise the partial response stands. A timed-out callback abandons its
exchange: the request finishes immediately, the timers it owns are cancelled,
and callbacks already queued for that exchange are skipped when they run, so
one runaway callback cannot wedge the server or waste event-loop slots.

### Backpressure

All requests and async callbacks share one bounded queue per directive
instance. When the queue is full:

- new requests get `503 Service Unavailable`.
- async callbacks are dropped and logged. The exchange records the failure, so
  a request that has not written a response yet also gets
  `503 Service Unavailable` instead of hanging.

Raise `queue_size` to absorb bursts, or make handlers cheaper. Requests are
serialized: JavaScript never runs concurrently within one instance, so a slow
handler delays the ones behind it.

Work outside the event loop is bounded separately: filesystem reads and writes,
`fetch` requests, and static file reads run in at most 64 goroutines per
instance. Work over that limit waits for a slot.

### Shutdown

When Caddy stops or reloads its configuration, the instance is cleaned up and
its event loop stops. The loop first drains what is already queued, running
those tasks in order for up to 5 seconds, then returns. Nothing new is accepted
once it starts stopping:

- New requests get `503 Service Unavailable`, because the loop no longer takes
  submissions.
- Async callbacks that land during the drain, from a pending `fetch`, `fs`, or
  timer, are dropped and logged as
  `javascript: event queue full, dropping async callback`. If the response is
  still unwritten, the exchange fails and the client gets `503 Service
  Unavailable` when possible; Caddy is shutting down anyway.
- Work still in flight outside the loop, such as an `os.Root` read or an
  outbound `fetch`, is abandoned when the drain deadline passes. Its callback
  arrives after the loop has stopped, so it is dropped.

### Reloading

With `watch` on, the plugin watches the script's directory and reloads when the
script is written, created, or renamed, debounced by 200ms.

- A reload builds a new VM and route table. In-flight exchanges keep using the
  program they started with.
- The background runtime reloads from the same source and fences jobs admitted
  before the swap; see below.
- A reload that fails to parse or throws logs
  `javascript: reload failed, keeping previous script` and leaves the old
  program serving.
- A failure during startup is fatal: Caddy refuses to start with a script that
  throws while loading.
- Watching needs functional inotify. Some network filesystems do not deliver
  events; use `watch false` there.

### Background runtime

Provisioning also loads the script into a resident background runtime: a second
VM with its own event loop, kept separate from request handling and from the
request program's state. It loads at provisioning, registers its handlers, and
then idles until shutdown; no JavaScript API reaches it yet. With `watch` on, it
reloads with the request runtime. Top-level script code therefore runs once per
runtime at startup and again on each reload, so top-level side effects such as
file writes happen twice each time.

The background runtime owns a bounded FIFO for jobs, sized by `queue_size`. A
single supervisor goroutine hands one job at a time to its event loop, so jobs
never run concurrently with one another. Admission never blocks: a full queue
rejects the job and increments `caddy_javascript_queue_full_total`.

Each successful reload builds a new Program and increments the runtime
generation. A job admitted under an older generation is fenced: it is cancelled
with a stale-generation error and never dispatched, so a callback created before
a reload cannot reach the new Program. A reload that fails to load leaves the
previous Program and generation in place, so queued jobs stay valid. On
shutdown the queue closes and the worker drains the jobs already admitted before
the event loop stops.

Each job carries a stable random id, a worker name, a serialized payload, a
generation, an optional absolute deadline, an async budget, and a retry policy
(`MaxAttempts`, `Backoff`). The supervisor drives the Go-side lifecycle:

- `pending` moves to `running` when an attempt starts, and the attempt count
  grows with each start.
- An executor that returns nil acks the job (`succeeded`); an error fails the
  attempt.
- A failed job retries while attempts remain, moving to `retrying` and waiting
  an exponential backoff (the base delay doubled per attempt, capped at 30s)
  before re-admission. The default policy is a single attempt, so retries are
  opt-in.
- `Cancel` stops a pending, retrying, or running job and cancels its context.
  The context is rooted at `context.Background`, so cancellation is independent
  of the request that produced the job.
- A job whose deadline passed before it starts fails without running. A
  deadline reached before a retry fails the job instead of retrying it, and the
  job context expires with the deadline.

No JavaScript API can enqueue or process a job yet.

## Status codes and errors

| Situation | Response |
|---|---|
| Route matched and wrote a response | Whatever the script sent. |
| No route or middleware matched, `fallthrough` off | `404 Not Found` (plain text). |
| No route or middleware matched, `fallthrough` on | Next HTTP handler. |
| Route matched, handler wrote nothing | `404 Not Found`. |
| Script throws or calls `next(err)`, nothing written | `500 Internal Server Error`. |
| `next(err)` with `err.status` or `err.statusCode`, nothing written | That status, with its plain text. |
| `next(err)` reaches an error handler | Whatever the error handler sent. |
| A body parser rejects the body, no error handler | `400`, `403`, or `413` from the error's status. |
| Handler exceeds `timeout`, nothing written | `504 Gateway Timeout`. |
| Handler exceeds `timeout` after writing | Partial response stands, error logged. |
| Async callback exceeds `timeout`, nothing written | `504 Gateway Timeout`; its timers are cancelled. |
| Async callback exceeds `timeout` after writing | Partial response stands, error logged. |
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
- **Admin eval.** The admin eval endpoint is off by default. Enabling `eval`
  lets anyone with admin API access run arbitrary code in the directive's
  runtime. Expose the admin API only on a trusted listener.

Deploy Caddy itself with the usual precautions: non-root user, read-only
filesystem where possible, and no admin API exposure. The bundled image runs as
root by default, like the upstream Caddy image.

## Admin eval

Enabling `eval` in a directive registers its script path as a target on the
Caddy admin API. The endpoint runs code on the directive's event loop:

```sh
curl -s localhost:2019/javascript/eval \
  -H 'Content-Type: application/json' \
  -d '{"target":"/srv/app/server.js","code":"1+1"}'
# {"result":"2"}
```

The target is the absolute script path. The endpoint is off unless the
directive opts in. It runs with the same globals and sandbox as the script and
is bounded by the request context. It is a diagnostic tool; treat it as remote
code execution.

## Metrics and logging

Caddy's Prometheus endpoint exposes:

| Metric | Type | Meaning |
|---|---|---|
| `caddy_javascript_requests_total{status}` | counter | Responses the handler completed, labelled by status. |
| `caddy_javascript_timeouts_total` | counter | Handler invocations and async callbacks interrupted by the watchdog. |
| `caddy_javascript_queue_full_total` | counter | Requests, callbacks, and jobs rejected or dropped because a queue was full. |
| `caddy_javascript_reloads_total` | counter | Successful script reloads. |

Log messages to look for:

- `javascript: reloaded script`
- `javascript: reload failed, keeping previous script`
- `javascript: handler error`
- `javascript: async callback error`
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
  small, but a full queue drops callbacks and fails their exchanges, so leave
  headroom.
- Filesystem, `fetch`, and static file work is bounded to 64 goroutines per
  instance. A burst beyond that queues instead of spawning more goroutines.
- Reload rebuilds the VM, so routes are re-registered from the script on every
  reload.

## Limitations and roadmap

Not implemented today:

- npm installation.
- Server-side streaming responses, SSE, and WebSocket upgrades.
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

Unit tests, format check, and vet are wrapped in `test/unit.sh`, which runs
them in the builder container under a bounded timeout:

```sh
./test/unit.sh
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

End-to-end suite. `test/e2e.sh` starts the image produced by `./build.sh`,
mounts the app fixture at `/app`, drives every endpoint over HTTP with `curl`,
and checks the results. It also runs provision-time validation against a set of
bad Caddyfiles. Build the image first; the runner does not build it:

```sh
./build.sh
E2E_IMAGE=caddy-javascript:test sh test/e2e.sh
```

`test/app` is the fixture. The runner copies it to a temp directory and mounts
that copy at `/app` so the reload check can rewrite the script without touching
the checkout. The image tag defaults to the repo slug in `NOTICE`; set
`E2E_IMAGE` to test another tag. The harness needs only `docker` and `curl` on
the host.

Manual testing uses the sample app in `www/test`:

```sh
cd www/test
./run.sh    # caddy run --config Caddyfile --adapter caddyfile
```

CI runs all of the above: format, build, vet, unit tests, the race detector,
the image build, and the e2e suite. See `.github/workflows/ci.yml`.

When adding request/response checks, add a `check` line to `test/e2e.sh`
instead of new Go code. The helper takes a name, method, path, and any of
`--status`, `--ctype`, `--equals`, `--contains`, `--header`, `--hdr`,
`--hdr-contains`, `--data`, and `--data-file`. `--equals` is an exact body
comparison; the other checks match.

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
test                 unit runner, end-to-end runner, and app fixture
spec.md              design target
```

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| Caddy will not start, `failed to initialize runtime` | The script throws while loading, or the path is wrong. Check the path and run it under `watch false` with logs at debug level. |
| `404` on a route you registered | Wrong path or method, or a handler returned without writing. Trailing slashes and case are ignored, so check for a typo or a missing `next()`. |
| `404` for everything, no script errors | Script failed to load and the handler has no program, or the directive is in a `handle` block whose matcher does not match. |
| `503 Service Unavailable` | Queue full. Raise `queue_size`, or find the slow handler blocking the loop. |
| `504 Gateway Timeout` | `timeout` is too small, or a handler or async callback has an infinite loop. |
| `413` | Body exceeds `max_body`. Raise it or reject large uploads in Caddy. |
| `fetch blocked: ...` | Host missing from `fetch_allow`, or `*` with a private resolution. Listed hosts are allowed verbatim. |
| `fs` `ENOENT` | Wrong `root`. It defaults to the script's directory, not the Caddy working directory. |
| Reload does nothing | `watch false`, no inotify from the filesystem, or the file was replaced without a matching event. Reload manually with `caddy reload`. |
| Unknown option | Typo in the block. Unknown keys are a parse error, not ignored. |
| `javascript` is not recognized | The binary was built without the plugin. Rebuild with `xcaddy --with github.com/frinknet/caddy-javascript`. |

## License

Apache-2.0. See `LICENSE` and `NOTICE`.