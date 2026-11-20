# caddy-javascript

Run ESM JavaScript inside Caddy as a native runtime. The target design is in
[`spec.md`](spec.md).

**Status: one VM per site.** A site block creates one
[go-quickjs](https://github.com/go-quickjs/go-quickjs) VM; its top-level
directives and its `handle` directives share that VM's realm, module cache, and
entry modules. A configuration reload reuses the site VM when its `root`,
`memory`, and `queue_size` are unchanged and no loaded module was edited, so a
route or mount edit keeps module state; `env_allow` and `fetch_allow` are
refreshed in the reused VM, and an edited module gets a fresh VM. The
global option evaluates a worker entry but does not dispatch requests. The
`http` object carries the request and its helpers, `route()` registers
JavaScript layers, and the global worker owns `config`, the whole Caddy
configuration. Workers, messaging, allowlisted fetch, filesystem access,
`watch()`, and Caddy events in the global worker are installed. Chunked
streams, server-sent events, and WebSocket upgrades are installed too. With
`watch` left on, editing a module the VM loaded reloads the active
configuration, and a rejected edit leaves the previous generation serving.

## What works today

- The `javascript` global option provisions a worker VM and evaluates its entry
  module.
- The `javascript` HTTP directive joins the VM of its enclosing site. The entry
  module must export a default function:
  `export default function handler(http) { ... }`.
- Directives in the same site block share one VM, keyed by the site's host
  matcher set. An explicit `site` name overrides that discovery. Provisioning
  order does not matter, and a module imported by two directives is evaluated
  once.
- A site VM is process-wide. A reload that leaves `root`, `memory`, and
  `queue_size` unchanged reuses it; a changed script loads into the same VM. A
  changed VM option provisions a new VM, and a removed site closes its VM when
  the last handler releases it.
- ESM imports resolve inside `root`. Bare specifiers, npm, CommonJS, and paths
  outside the root are rejected.
- Each request gets a fresh `http` object with `method`, `url`, `path`,
  `query`, `headers`, `cookies`, `body`
  (`text`/`json`/`bytes`, bounded at 1 MiB), `host`, `remote`, `protocol`,
  `vars`, `rewrite`, `respond`, `stream`, `sse`, `websocket`, and `signal`.
- During a dispatch the global space is the request's Caddy placeholder space.
  Reading a bare identifier resolves a `map` destination or any other
  placeholder (`site`, `host`, `method`, `path`); it never reads a variable.
  Assigning one, including a new name, overrides the matching placeholder, so
  downstream `{name}` sees it, but it does not create a variable, so
  `{http.vars.name}` does not. Writes are request-scoped and values must be
  plain data; assigning a function is rejected. A local declaration shadows the
  name. Outside a dispatch unresolved names are not defined, and during one
  they read as `undefined` instead of throwing. `globalThis.name = value` is an
  ordinary VM property and does not sync; use a bare assignment or `http.vars`.
- `http.vars` is a live `Proxy` over the request's Caddy variable table:
  `http.vars.name = value` writes the table, as Caddy's `vars` directive does,
  and downstream handlers and `{http.vars.name}` see it. It does not create a
  bare placeholder, and a bare read never sees a variable. The table is flat: a
  map-valued var is one key. `http.vars.user.name` works in JavaScript, but a
  template reads the whole value with
  `{{placeholder "http.vars.user"}}`; there is no child placeholder.
- `http.headers` is a live `Proxy` over the request's headers. Read with
  `http.headers["x-app"]`, set with `http.headers["x-app"] = "caddy"`, and
  remove with `delete http.headers["x-app"]`; `"x-app" in http.headers`
  reports presence and `Object.keys` lists the names. Header names are
  case-insensitive, and repeated request headers read as an array.
- `http.rewrite({method, path, headers})` mutates the real request for
  downstream Caddy handlers. Fields on `http` are a snapshot taken at dispatch,
  so a rewrite does not change the current `http.path`.
- `route(matcher, fn)` registers module-scope JavaScript layers. They run in
  registration order before the module's default export. A layer that returns
  without responding continues to the next matching layer; a layer that
  responds stops the chain, so the default export does not run. A string
  matcher matches a path exactly, or a prefix when it ends in `*`; a function
  matcher receives `http`.
- `_` is a global object of template functions. It exposes the sprig function
  map plus mirrored Caddy built-ins (`include`, `httpInclude`, `placeholder`,
  `ph`, `readFile`, `fileExists`, `fileStat`, `listFiles`, `stripHTML`,
  `markdown`, `splitFrontMatter`, `humanize`, `pathEscape`, `httpError`,
  `httpStatus`, `env`, `expandenv`, and `getHostByName`). Functions that need a
  request (`include`, `httpInclude`, `placeholder`, `ph`, `httpError`, and the
  file helpers) throw without one. `env` and `expandenv` require `env_allow`;
  `httpInclude` and `getHostByName` require `fetch_allow`. File helpers read the
  sandbox root. `fileStat` returns `{Name, Size, Mode, ModTime, IsDir}` with an
  RFC 3339 mod time. `splitFrontMatter` returns `{Meta, Body}`. `humanize`
  accepts `size` and `time[:layout]` formats. `time.Duration` accepts a duration
  string or nanoseconds; `time.Time` accepts a Date or RFC 3339.
- A site's `templates` extensions are also exposed on `_`. Functions declared
  under `templates { extensions { ... } }` in the same site are loaded into the
  registry, so a script and a template call the same implementation. The
  extension modules are provisioned a second time for the VM; keep them free of
  per-process side effects.
- `config` is the global worker's copy of the whole Caddy configuration, plus
  `config.apply()` and `config.caddyfile(path)`. Site VMs have no `config`
  object. The worker seeds `config` from the admin endpoint once Caddy is
  listening, so a module-scope read during the first load may see it as
  undefined. Use it from a timer, event, or message, or check
  `globalThis.config`.
- `config.apply()` serializes `config`, validates that `apps.javascript` is
  still present, and loads it through the admin endpoint. It returns `true`
  when it started a load, and `false` when the document is unchanged or
  another load is already in flight. A failure is logged; the old
  configuration keeps serving. The admin address comes from `CADDY_ADMIN`,
  defaulting to `localhost:2019`; with the admin endpoint disabled, `config`
  is not seeded and `apply()` cannot run.
- The reload reuses the global worker VM, so module state, timers, and ports
  survive `config.apply()`. Site VMs are reused too when the site's
  `root`, `memory`, and `queue_size` are unchanged; a changed script loads a
  new module into the same VM. A changed option or a removed site closes the
  VM when the last handler releases it.
- An edit at module scope (top-level code) is a configuration error, because
  every reload would edit and reload again. Edit `config` from a timer, event,
  or message.
- `config.caddyfile(path)` adapts a Caddyfile inside the ESM root and returns a
  plain config object. It does not apply anything and does not modify `config`.
  Adaptation warnings are logged. Caddy resolves the file's `import`s relative
  to the file, so imported files can leave the ESM root; treat the path as
  trusted input.
- `events` is installed only in the global worker. `events.on(name, fn)`
  registers a listener at module scope, and `events.on("*", fn)` listens to
  every event. Caddy accepts subscriptions only before its apps start, so a
  registration after module evaluation is an error. A listener receives
  `{ id, name, origin, time, data }`: `origin` is `caddy` for core events, and
  `data` is a plain clone of the event payload. Events are delivered on the VM
  loop, so listeners run after the emitter returns. Each reload binds the
  worker's listeners again; a changed worker module replaces the old ones.
- `respond` writes the status, headers, and body. A handler that returns without
  responding calls the next Caddy handler; use `respond(404, ...)` to answer
  404 explicitly.
- `http.signal` is the request's `AbortSignal`. It aborts when the client
  disconnects, the handler deadline ends, or the request finishes, so async work
  can stop instead of outliving the request. The signal is tied to the dispatch
  through the runtime's async context, so it still applies after `await` and
  inside timers started by that dispatch. `fetch()` uses it automatically: a
  call with no `signal` option is cancelled with the request, and a signal the
  script passes itself takes precedence.
- `http.stream(status, headers)` starts a chunked response and returns a writer
  with `write(chunk)`, `end(chunk?)`, and `close()`. Every chunk is flushed, so
  a client sees it immediately. The writer stays usable after the dispatch
  returns, which lets a timer send a slow body; the handler waits until the
  stream ends or the client disconnects. `http.respond()` after a stream throws.
- `http.sse()` starts a `text/event-stream` response and returns
  `send(data, {event, id, retry})`, `comment(text)`, and `close()`. A string is
  sent as-is and any other value is JSON-encoded; multi-line data becomes one
  `data:` line per line.
- `http.websocket()` upgrades a WebSocket request and returns a socket with
  `send(data)`, `close(code?, reason?)`, `onmessage`, and `onclose`. A string is
  sent as a text frame and a `Uint8Array` as a binary frame. The socket keeps
  the connection alive after the dispatch returns; `onmessage(data)` runs on
  the VM loop, and `onclose(code, reason)` runs when either side closes.
  Fragmented messages are reassembled, client frames must be masked, and a
  message is capped at 1 MiB.
- Multiple `javascript` directives in one site block form an ordered chain and
  share the site VM. Each module's default export runs at its directive
  position; a handler that responds stops the chain, and one that returns
  without responding continues to the next directive.
- Reading `http.body` consumes the request stream, so a later Caddy handler sees
  an empty body.
- `timeout` interrupts a runaway handler and answers `504`. A full per-VM queue
  answers `503`. A thrown exception, including the per-request callback budget,
  answers `500`.
- One request may make at most 10,000 host callbacks (header, body, variables,
  rewrite, and template bridges). A loop that keeps calling into Go fails with
  `request callback budget exceeded` instead of pinning the VM goroutine.
- `memory` and `queue_size` are active. A reload closes an old VM only when a
  VM option changed, a loaded module was edited, or the site is gone. An
  edited module gets a fresh VM; the previous generation keeps its own until
  its handlers release it.
- Shutdown stops new requests, waits up to 5s for admitted requests to finish,
  then closes the loop and its workers.
- Entry evaluation has a provision deadline. A syntax error, a rejected
  top-level await, or a runaway module fails the configuration load, so the
  previous generation keeps serving.
- `fs` is available as the `fs` module. The sandbox root is `/` to the script;
  `..` and symlinks that leave the root are refused, and writes stay inside it.
- `watch(dir, fn)` watches one sandbox directory without recursing, debounces
  changes by 100ms, and calls `fn(names)` with the changed paths. It returns a
  function that stops the watch. The callback runs on the VM loop. Watches stop
  when the VM closes or its module is replaced.
- `fetch` is installed only when `fetch_allow` is non-empty. Every request,
  including each redirect, is checked against the allowlist; response bodies
  are capped at 1 MiB. A fetch issued during a dispatch inherits
  `http.signal`, so a disconnect or deadline cancels it even without an explicit
  signal; `_.httpInclude` and `_.getHostByName` use the same list.
- `structuredClone`, `MessageChannel`, `MessagePort`, and `BroadcastChannel`
  are installed. Broadcast names are scoped to the VM, so an old and a new
  generation never exchange messages, and a script still reads the name it
  chose. A port keeps the loop alive until it is closed or `unref`d.
- `Worker` starts a runtime on its own goroutine with the same root,
  capabilities, and module loader. A worker specifier resolves against the
  entry module inside the sandbox; a bare specifier or one that leaves the root
  is refused. At most 8 workers run at once; starting one past the limit throws
  in the script, and closing the VM terminates the rest.
- `watch [true|false]` reloads the active configuration when a module the VM
  loaded changes. The entry module and its imports are watched, the 150ms
  debounce coalesces one save, and concurrent handlers share one reload. A file
  with a syntax error is rejected by the admin load, which is logged, and the
  previous generation keeps serving until the file is fixed.
- Requests and failures are counted per VM and logged at debug level with
  method, path, status, queue depth, and duration. Queue-full and timeout
  answers are warnings; an automatic reload is logged with its paths; a
  rejected edit is a warning and a failed `config.apply()` is an error.
- `process`, `require`, `run`, `serve`, `sockets`, and DNS are not installed.

A global worker can add a route and reload Caddy without losing its own
state:

```js
setTimeout(() => {
	const servers = globalThis.config?.apps?.http?.servers ?? {}
	for (const name of Object.keys(servers)) {
		servers[name].routes ??= []
		servers[name].routes.unshift({
			match: [{ path: ["/global/*"] }],
			handle: [{ handler: "static_response", body: "global" }],
		})
	}
	config.apply()
}, 1000)
```

`site`, `root`, `timeout`, `memory`, `queue_size`, `env_allow`, `fetch_allow`,
and `watch` are active.

## Caddyfile

```caddyfile
{
	javascript global.js
}

example.com {
	javascript site.js
}
```

```caddyfile
example.com {
	handle /api/* {
		javascript api.js
	}
}
```

Options are available on both the global option and the directive:

| Option | Meaning | Default |
|---|---|---|
| `site <name>` | Explicit site key. Overrides host matcher discovery. | discovered from the enclosing route |
| `root <dir>` | Sandbox root. | entry module's directory |
| `timeout <duration>` | Deadline for one handler call. | `3s` |
| `memory <bytes>` | Per-VM memory limit. | `64MiB` |
| `queue_size <n>` | Per-VM admission queue. | `256` |
| `env_allow <names...>` | Environment names visible to `_`. | empty |
| `fetch_allow <hosts...>` | Outbound hosts for `fetch`, `_.httpInclude`, and `_.getHostByName`. | empty |
| `watch [true\|false]` | Reload on module-tree changes. | `true` |

```caddyfile
example.com {
	javascript /srv/app/site.js {
		root        /srv/app
		timeout     2s
		memory      67108864
		queue_size  8
	}
}
```

## JavaScript API

```js
import { label } from "./lib.js"

route("/api/*", (http) => {
	http.headers["x-app"] = "caddy"
})

export default function handler(http) {
	const body = http.body.text()
	http.vars.seen = http.query.q ?? "-"
	http.respond(200, { "content-type": "text/plain" }, label(http.method, http.path) + body)
}
```

A stream, SSE response, or WebSocket stays live after the dispatch:

```js
export default function handler(http) {
	if (http.path === "/events") {
		const sse = http.sse()
		let n = 0
		const timer = setInterval(() => {
			if (++n > 5) {
				clearInterval(timer)
				sse.close()
				return
			}
			sse.send(n, { event: "tick" })
		}, 1000)
		return
	}
	if (http.path === "/socket") {
		const ws = http.websocket()
		ws.onmessage = (data) => ws.send("echo:" + data)
		ws.onclose = () => console.log("closed")
		return
	}
	const out = http.stream(200, { "content-type": "text/plain" })
	out.write("first ")
	setTimeout(() => out.end("second"), 100)
}
```

Timers, promises, `fs`, `watch`, `fetch` (with `fetch_allow`), workers,
`MessageChannel`, `BroadcastChannel`, `structuredClone`, and `events` in the
global worker are installed. A handler or route layer normally finishes
synchronously: a promise returned by a handler is not awaited, and
`respond()` after the dispatch has returned throws. A stream or WebSocket is
the exception: its writer and callbacks stay live after the dispatch returns,
bounded by the client connection rather than the handler `timeout`. Start
other asynchronous work from module scope, a timer, an event listener, or a
worker, and read its result on a later request.

## JSON configuration

The app ID is `javascript`. The handler ID is `http.handlers.javascript`.
Options use the same names as the Caddyfile (`script_path`, `site`, `root`,
`timeout`, `memory`, `queue_size`, `env_allow`, `fetch_allow`, `watch`).

## Build and test

Every Go command runs in Docker. There is no host Go toolchain.

```sh
sh test/unit.sh
./build.sh
sh test/e2e.sh
```

The e2e layer runs the image `./build.sh` produces. It checks the image's Caddy
and go-quickjs versions against `go.mod`, drives the site handler over HTTP, and
checks that invalid configurations fail `caddy validate`.

## Layout

- `javascript.go` is the only Go file at the repository root. It blank imports
  the packages that register their modules.
- `pkg/config` holds the option schema and Caddyfile parsing.
- `pkg/vm` holds the go-quickjs runtime, sandbox loader, event loop,
  per-request dispatch, and the worker `config` object.
- `pkg/handler` holds the app, the HTTP handler, the directive registrations,
  the site VM pool, and the admin reload path.
- `test/app` holds the e2e fixture modules.
- `test/e2e.sh` and `test/unit.sh` are the test entry points.
- `www/Caddyfile` is the shipped image configuration.
- `spec.md` is the target design; it is not a description of working code.

## Non-goals

- Node compatibility, CommonJS, npm, Express, or a vendored `node_modules`.
- cgo, V8, or a C toolchain.
- One VM per request.

## License

See [`LICENSE`](LICENSE).