# caddy-javascript

Run ESM JavaScript inside Caddy as a native runtime. The target design is in
[`spec.md`](spec.md).

**Status: one VM per site.** A site block creates one
[go-quickjs](https://github.com/go-quickjs/go-quickjs) VM per configuration
generation; its top-level directives and its `handle` directives share that VM's
realm, module cache, and entry modules. The global option evaluates a worker
entry but does not dispatch requests. The `http` object carries the request and
its helpers, and `route()` registers JavaScript layers. Workers, messaging,
`server`, `_`, events, fetch, and filesystem access are not implemented yet.

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
- The site VM belongs to one configuration generation. A reload provisions a
  new registry and new VMs; an old generation never shares a VM with a new one.
- ESM imports resolve inside `root`. Bare specifiers, npm, CommonJS, and paths
  outside the root are rejected.
- Each request gets a fresh `http` object with `method`, `url`, `path`,
  `query`, `params`, `headers` (`get`/`set`/`has`/`delete`), `cookies`, `body`
  (`text`/`json`/`bytes`, bounded at 1 MiB), `host`, `remote`, `protocol`,
  `vars`, `rewrite`, `respond`, and `next`.
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
- `http.rewrite({method, path, headers})` mutates the real request for
  downstream Caddy handlers. Fields on `http` are a snapshot taken at dispatch,
  so a rewrite does not change the current `http.path`.
- `route(matcher, fn)` registers module-scope JavaScript layers. They run in
  registration order before the module's default export, and a layer calls
  `next()` (or `http.next()`) to continue. A layer that returns without calling
  `next()` stops the chain, so the default export does not run. A string
  matcher matches a path exactly, or a prefix when it ends in `*`; a function
  matcher receives `http`. `params` is reserved for Caddy path captures and is
  empty today.
- `respond` writes the status, headers, and body. A handler that returns without
  responding answers `404`, or calls the next Caddy handler when `fallthrough`
  is set.
- Reading `http.body` consumes the request stream, so a later Caddy handler sees
  an empty body.
- `timeout` interrupts a runaway handler and answers `504`. A full per-VM queue
  answers `503`. A thrown exception answers `500`.
- `memory` and `queue_size` are active. A reload closes the old VMs.
- Entry evaluation has a provision deadline. A syntax error, a rejected
  top-level await, or a runaway module fails the configuration load, so the
  previous generation keeps serving.

Parsed but not active yet: `env_allow`, `fetch_allow`, and `watch`.
`site`, `root`, `timeout`, `memory`, `queue_size`, and `fallthrough` are active.

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
| `env_allow <names...>` | Environment allowlist. Parsed, not active yet. | empty |
| `fetch_allow <hosts...>` | Outbound host allowlist. Parsed, not active yet. | empty |
| `watch [true\|false]` | Reload on module-tree changes. Parsed, not active yet. | `true` |
| `fallthrough` | Continue the Caddy chain when the handler has no response. | off |

```caddyfile
example.com {
	javascript /srv/app/site.js {
		root        /srv/app
		timeout     2s
		memory      67108864
		queue_size  8
		fallthrough
	}
}
```

## JavaScript API

```js
import { label } from "./lib.js"

route("/api/*", (http, next) => {
	http.headers.set("x-app", "caddy")
	next()
})

export default function handler(http) {
	const body = http.body.text()
	http.vars.seen = http.query.q ?? "-"
	http.respond(200, { "content-type": "text/plain" }, label(http.method, http.path) + body)
}
```

Timers are installed. Handlers and route layers must finish synchronously: a
promise returned by a handler is not awaited, and a script that responds after
the dispatch has returned gets an error. `server`, `_`, `fetch`, `fs`, workers,
`MessageChannel`, and Caddy events are not available yet.

## JSON configuration

The app ID is `javascript`. The handler ID is `http.handlers.javascript`.
Options use the same names as the Caddyfile (`script_path`, `site`, `root`,
`timeout`, `memory`, `queue_size`, `env_allow`, `fetch_allow`, `watch`,
`fallthrough`).

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
- `pkg/vm` holds the go-quickjs runtime, sandbox loader, event loop, generation
  registry, and per-request dispatch.
- `pkg/handler` holds the app, the HTTP handler, and the directive
  registrations.
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