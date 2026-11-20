# caddy-javascript

Run ESM JavaScript inside Caddy as a native runtime. The target design is in
[`spec.md`](spec.md).

**Status: one VM per site.** A site block creates one
[go-quickjs](https://github.com/go-quickjs/go-quickjs) VM; its top-level
directives and its `handle` directives share that VM's realm, module cache, and
entry modules. A configuration reload reuses the site VM when its `root`,
`memory`, and `queue_size` are unchanged, so a route or mount edit keeps module
state; `env_allow` and `fetch_allow` are refreshed in the reused VM. The
global option evaluates a worker entry but does not dispatch requests. The
`http` object carries the request and its helpers, `route()` registers
JavaScript layers, and the global worker owns `config`, the whole Caddy
configuration. Workers, messaging, events, fetch, and filesystem access are not
implemented yet.

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
  `vars`, `rewrite`, and `respond`.
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
- `respond` writes the status, headers, and body. A handler that returns without
  responding calls the next Caddy handler; use `respond(404, ...)` to answer
  404 explicitly.
- Multiple `javascript` directives in one site block form an ordered chain and
  share the site VM. Each module's default export runs at its directive
  position; a handler that responds stops the chain, and one that returns
  without responding continues to the next directive.
- Reading `http.body` consumes the request stream, so a later Caddy handler sees
  an empty body.
- `timeout` interrupts a runaway handler and answers `504`. A full per-VM queue
  answers `503`. A thrown exception answers `500`.
- `memory` and `queue_size` are active. A reload closes an old VM only when a
  VM option changed or the site is gone.
- Entry evaluation has a provision deadline. A syntax error, a rejected
  top-level await, or a runaway module fails the configuration load, so the
  previous generation keeps serving.

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

Parsed but not active yet: `watch`.
`site`, `root`, `timeout`, `memory`, `queue_size`, `env_allow`, and
`fetch_allow` are active.

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
| `fetch_allow <hosts...>` | Outbound hosts for `_.httpInclude` and `_.getHostByName`. | empty |
| `watch [true\|false]` | Reload on module-tree changes. Parsed, not active yet. | `true` |

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

Timers are installed. Handlers and route layers must finish synchronously: a
promise returned by a handler is not awaited, and a script that responds after
the dispatch has returned gets an error. `fetch`, `fs`, workers,
`MessageChannel`, and Caddy events are not available yet.

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