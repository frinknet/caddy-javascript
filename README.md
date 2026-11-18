# caddy-javascript

Run Express-style JavaScript inside Caddy. The plugin embeds
[Sobek](https://github.com/grafana/sobek), a pure-Go ECMAScript engine, and
drives it from a single-threaded event loop, so scripts run in-process with no
CGO, no V8, and no external runtime.

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

## Configuration

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

`process.env` is empty unless names are listed in `env_allow`. `fetch` is
denied unless the host is listed in `fetch_allow` (use `*` to allow any public
host). Script changes are reloaded automatically while `watch` is on; a reload
that fails to parse keeps the previous script running.

## JavaScript API

| Global | Description |
|---|---|
| `app.get(path, fn)` / `app.post(path, fn)` | register a route (exact path match) |
| `app.use(fn)` | fallback handler for unmatched requests |
| `req.path`, `req.method`, `req.host` | request line |
| `req.headers`, `req.header(name)` | request headers (first value) |
| `req.query`, `req.queryAll` | query params, first value or all values |
| `req.body` | JSON, `+json`, or form-urlencoded body, else the raw string |
| `res.status(code)` | set the status code, chainable |
| `res.set(name, value)`, `res.type(t)` | set a header / content type |
| `res.send(data)`, `res.json(obj)`, `res.end([data])` | send a response |
| `res.redirect([code,] url)`, `res.flush()` | redirect / flush for streaming |
| `setTimeout(fn, ms)` | timer that fires back on the event loop |
| `fetch(url, cb)` | async GET, `cb(err, res)` with `res.status` and `res.text` |
| `fs.readFile` / `fs.readFileSync` / `fs.writeFile` / `fs.writeFileSync` | sandboxed file access |
| `path.join(...parts)` | POSIX path join |
| `process.env` | allowlisted environment variables |
| `require('fs')`, `require('path')` | module shims |

`fs` paths are confined to `root` with `os.Root`, which blocks `..` and symlink
escapes. Responses stay open until they are written, so `fetch`, timers, and
async `fs` callbacks can complete the response.

Metrics are exposed on the Caddy metrics endpoint as
`caddy_javascript_requests_total{status}`, `caddy_javascript_timeouts_total`,
`caddy_javascript_queue_full_total`, and `caddy_javascript_reloads_total`.

## Status

Implemented: routing, request/response binding, `fetch` with host allowlist,
timers, sandboxed `fs`, `path`, `require`, `env_allow`, `max_body`,
`fallthrough`, per-request timeout with interrupt recovery, hot reload with
rollback, graceful drain, structured logging, and Prometheus metrics.

Planned: `app.put` / `app.delete` / `app.all`, path parameters and wildcards,
multipart bodies, and `require` of local modules.


## Building

The plugin is pure Go and cross-compiles with `CGO_ENABLED=0` via `xcaddy`:

```sh
xcaddy build --with github.com/frinknet/caddy-javascript
```

`./build.sh` builds the Docker image from this repo instead.

## Development

All Go tooling runs in Docker; the host needs no toolchain. The
`caddy-sf-mod` volume holds the Go caches.

```sh
docker run --rm \
  -v caddy-sf-mod:/mod \
  -v "$PWD":/src -w /src \
  -e GOMODCACHE=/mod -e GOCACHE=/mod/gocache -e CGO_ENABLED=0 \
  caddy:2.11.9-builder-alpine \
  sh -c 'gofmt -l . && go build ./... && go vet ./...'
```

Unit tests (the race detector needs cgo, so install a compiler first):

```sh
docker run --rm \
  -v caddy-sf-mod:/mod \
  -v "$PWD":/src -w /src \
  -e GOMODCACHE=/mod -e GOCACHE=/mod/gocache -e CGO_ENABLED=1 \
  caddy:2.11.9-builder-alpine \
  sh -c 'apk add --no-cache gcc musl-dev && go test -race ./...'
```

End-to-end tests build the plugin image and drive it over HTTP:

```sh
tests/e2e/run.sh
```

## Layout

```
javascript.go        Caddy module glue (registration, Provision, ServeHTTP)
pkg/config           options and Caddyfile parsing
pkg/sched            task queue, run loop, backpressure, drain
pkg/bridge           Sobek runtime, JS globals, req/res, watchdog
pkg/router           route table and dispatch
pkg/files            sandboxed fs, path.join, file watching
www/test             sample Caddyfile and app
tests/e2e            end-to-end harness
```

## License

Apache-2.0. See `LICENSE` and `NOTICE`.
