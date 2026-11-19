# Technical Specification: Caddy JavaScript Runtime

Status: target design. The first runtime slice is implemented: a per-directive
go-quickjs VM, ESM evaluation with a sandboxed loader, and a minimal `http`
object. The rest of this file is the target. `README.md` documents what runs
today.

## 1. Vision

Run JavaScript as a first-class Caddy runtime, not as an Express emulation.

- One warm VM per site. A site is the unit of identity, configuration, and
  state. The entry module and its imports are compiled and evaluated once per
  configuration generation.
- One warm VM per worker. A worker has its own scope, its own event loop, and
  no HTTP surface.
- A fresh `http` object per request. There is no per-request VM and no
  per-request parse.
- Native Caddy concepts at the edges: routes, matchers, rewrites, responses,
  placeholders, templates, events, and the admin configuration.
- ESM only. No CommonJS, no npm, no Node compatibility layer.

The runtime is [go-quickjs](https://github.com/go-quickjs/go-quickjs), a pure-Go
QuickJS-NG reimplementation, pinned to `v0.40.0`. It needs no cgo, no C
toolchain, and no C engine compiled to WebAssembly. The JIT stays off for now;
the interpreter is enough, and the JIT would add a build tag and executable
memory requirements.

## 2. Runtime and concurrency

A VM is one `*quickjs.Runtime` on one goroutine. The runtime is not safe for
concurrent use; the VM's loop is the only goroutine that touches it.

- `server` VMs serve HTTP for one site and hold the site's JavaScript state.
- `worker` VMs have no HTTP surface. They receive messages, timers, events, and
  filesystem or network work posted to their loop.
- The `global.js` VM is a worker. It is not a shared realm; site VMs cannot see
  its values. It coordinates by messages, or by each VM evaluating `global.js`
  as a library.

Each VM has:

- A bounded admission queue. A full queue rejects HTTP with `503` and drops or
  reports worker work according to policy.
- A context deadline for every host call (`CallContext`, `EvalContext`).
- A per-runtime memory limit and stack limit.
- One `stdlib.Loop` for timers, ports, and work that finishes on other
  goroutines.

Many requests and events may be in flight in one VM. Promise jobs, timers, and
`AsyncWork` are interleaved by the loop, as in a browser. The request associated
with a callback is carried by the runtime's async context, set before invoking
the entry lambda and inherited by promise reactions and posted work.

Synchronous JavaScript still runs on the VM goroutine. A CPU-bound handler
delays every other request in that VM. This is inherent to the one-VM-per-site
model and is documented in the README.

## 3. Caddyfile surface

### 3.1 Global worker

```caddyfile
{
	javascript global.js
}
```

`javascript` in the global options block registers a `javascript` app. The app
provisions one worker VM for the entry module. The global worker has no HTTP
handlers. It may listen for Caddy events, run timers, own file watches, and
exchange messages with site VMs and other workers.

The global option may appear once. A second occurrence is a configuration
error.

### 3.2 Site VM

```caddyfile
example.com {
	javascript site.js
}
```

Inside a site block, `javascript site.js` creates the site VM. The entry module
is evaluated once. It may import other modules and call `route()` to register the
site's default JavaScript chain.

A site block has at most one site entrypoint. A `javascript` directive inside a
`handle`, `route`, or `handle_path` block is a handle lambda, not a second site
entrypoint.

### 3.3 Handle lambda

```caddyfile
example.com {
	handle /api/* {
		javascript api.js
	}
	handle {
		javascript site.js
	}
}
```

A `javascript` directive inside a matcher block loads its module into the
enclosing site VM and binds the module's default export as the lambda for the
Caddy route that contains the directive. The lambda runs when Caddy routes a
request to that handler. It shares the site VM's realm, module cache, `server`
object, and timers.

Handle directives are evaluated inside the site VM by construction, so they
inherit the site's identity without inspecting the request. At provisioning
time the handler resolves the site VM as follows:

1. If the directive sets `site <name>`, use that name.
2. Otherwise, find the enclosing top-level route by walking the server's routes
   (`ctx.Value(caddyhttp.ServerCtxKey)`) and matching this handler instance.
   Key the VM by that route's host matcher set.
3. If there is no host matcher (a catch-all site or a named route), fall back to
   the route index and, for named routes, the named route key.

The registry is scoped to one configuration generation. A handle directive
joins an existing site VM or creates it, so provisioning order does not matter.

### 3.4 Options

| Option | Meaning |
|---|---|
| `site <name>` | Explicit site key. Overrides route discovery. |
| `root <dir>` | Sandbox root. Default: the entry module's directory. |
| `timeout <duration>` | Deadline for one host call. Default `3s`. |
| `memory <bytes>` | Per-VM memory limit. Default `64MiB`. |
| `queue_size <n>` | VM admission queue. Default `256`. |
| `env_allow <names...>` | Environment names visible to the VM. |
| `fetch_allow <hosts...>` | Outbound hosts the VM may reach. |
| `watch [true\|false]` | Reload the site VM when the entry module tree changes. Default true. |
| `fallthrough` | Continue the Caddy chain when the JS chain produces no response. |

A negative or malformed value is a configuration error. The script file is
resolved at provisioning time; a missing file is an error, not a runtime 404.

## 4. JavaScript surface

### 4.1 `server`

`server` is the site's configuration draft. It is a mutable JavaScript object
seeded from the site's route subtree in the active Caddy config. It is not the
global object, and it is not the live Go configuration.

- Mutations are recorded in the draft. Before the next dispatch, the VM
  recompiles its JavaScript router from the draft, so `server.routes` edits take
  effect for subsequent requests without a Caddy reload.
- The draft covers the site's routes and handlers only. Listener addresses, TLS
  policies, storage, and other servers are read-only or absent.
- `server.apply()` sends the site subtree to Caddy. It is the only way a change
  to Caddy-native handlers, matchers, or TLS takes effect. Apply is a config
  load: the VM is replaced and all state is lost. It is fire-and-forget from the
  calling VM; a promise cannot resolve in a VM that is being torn down.
- Apply runs off the VM goroutine. `Handler.Cleanup` waits for the VM loop, so a
  load started on that loop deadlocks.
- Apply is serialized process-wide, canonicalized, and skipped when the site
  subtree is unchanged. A script that mutates and applies unconditionally in
  top-level code reloads forever; that is a configuration error.
- The live `*caddyhttp.Server` is never mutated from JavaScript. Caddy
  provisions and compiles routes and handler chains once, and request goroutines
  read them concurrently.

### 4.2 `http`

Every request receives a fresh `http` object. It carries:

- `method`, `url`, `path`, `query`, `headers`, `params`, `body`, `remote`,
  `protocol`, `host`.
- `vars`, a live view of the request's Caddy variable table.
- `respond(status, headers, body)`, terminal.
- `rewrite({method, path, headers})`, which mutates the real `*http.Request`
  (`URL.Path`, `RawPath`, `RequestURI`, headers) and continues.
- `next()`, which advances the JavaScript chain.
- `server`, the site's `server` object.
- `signal`, cancelled when the client disconnects or the request deadline ends.

### 4.3 Chain

Two chains, kept separate:

1. The Caddy middleware chain. Each `javascript` directive is one handler at the
   configured order. It either responds or calls Caddy's `next`.
2. The VM chain. `route(matcher, fn)` registers ordered JavaScript layers.
   `fn(http, next)` runs them. `next()` advances the VM chain.

When the VM chain finishes without writing a response, the handler calls Caddy's
`next` if `fallthrough` is set; otherwise it answers `404`.

- `route(matcher, fn)` is module-scope registration only.
- `http.respond()` and `http.rewrite()` are per-request actions only.
- `handle` matchers are the coarse entry point. `route()` is fine dispatch
  inside an entry. Do not duplicate the same path logic in both.
- A rewrite affects downstream Caddy handlers only. It cannot make a different
  Caddy route match, because route selection already happened.
- The site handler is ordered before `file_server` and after `encode` and
  `templates` so that JavaScript output flows back through compression and
  templating, and so that a fallthrough can reach the file server.

### 4.4 Request and response helpers

The runtime provides a small, browser-shaped surface, not Express:

- `http.headers.get/set/has/delete`
- `http.body.text/json/bytes`, bounded by `max_body`
- `http.cookies`
- `http.respond()` with status, headers, and string, bytes, or JSON body
- `http.rewrite()`
- `http.next()`
- `http.signal`

There is no `res.status().json()`, no `app.use`, no `req.param`, and no view
engine protocol. Templates are reached through `_`, not through a response
helper.

### 4.5 Vars

`http.vars` is a `Proxy` over `caddyhttp.GetVar` and `caddyhttp.SetVar`, the
request's `VarsCtxKey` table. Assignments are visible to downstream handlers and
to `{http.vars.*}` in templates. The view is per request.

Worker and global values do not become request vars by themselves. If a worker
value must reach templates, the site handler copies it into each request's var
table at dispatch.

### 4.6 Timers, promises, fetch, fs, watch

- `setTimeout`, `setInterval`, `queueMicrotask`, and promises are provided by
  `stdlib.Loop` and the engine's job queue.
- `fetch` comes from `stdlib.Fetch`, gated by `fetch_allow`, with a response
  body limit. No `fetch` without the capability.
- `fs` comes from `stdlib.FS`, confined to `root` with `os.Root` semantics and
  optional read-only mode.
- `watch(dir, fn)` uses `fsnotify` on the VM's goroutine. Callbacks are posted
  to the VM, debounced, and cancelled when the generation ends. The watcher is
  owned by the VM, not by the request.

### 4.7 `_`

`_` exposes template functions as synchronous JavaScript functions.

- The registry is the sprig function map plus Caddy's built-in template
  functions plus functions loaded from the site's `templates` extensions. Caddy
  does not export its built-in map; until it does, the plugin mirrors the
  built-ins and keeps them in sync. Prefer an upstream getter when available.
- Calls use reflection: JavaScript values are converted to the declared Go
  parameter types, and results are converted back to plain JavaScript values.
- `time.Duration` accepts a duration string or nanoseconds. `time.Time` accepts
  a JavaScript `Date` or RFC 3339. `template.HTML` and its siblings become
  strings. Structs, maps, and slices cross as plain objects and arrays.
- Functions that need a request (`placeholder`, `include`, `httpInclude`,
  `httpError`, and the file helpers) throw a clear error when called from a
  worker, a timer, or another context with no request.
- File helpers are rooted at the sandbox, not at Caddy's configured
  `file_root`.
- `env`, `expandenv`, and `getHostByName` from sprig are denied unless the
  matching capability is enabled. `httpInclude` is gated by `fetch_allow`.
- Blocking functions run on the VM goroutine and stall the site. The per-call
  deadline is the same as any host call.

### 4.8 Messaging and workers

Use go-quickjs's real messaging, not a handwritten clone:

- `MessageChannel`, `MessagePort`, `BroadcastChannel`, `structuredClone`,
  transfer lists, shared `ArrayBuffer`, and V8-shaped `DataCloneError`.
- `Worker` from `stdlib.Workers`, each worker a runtime on its own goroutine,
  installed with the same capability configuration. `Workers.New` and
  `Workers.Load` are host callbacks: they enforce the sandbox root, the module
  loader, and the options. `Workers.Max` bounds concurrency.
- Ports keep a loop alive until closed or `unref()`d. VM teardown closes every
  port and terminates workers.
- `BroadcastChannel` is process-global by name. Names are prefixed with the
  configuration generation so old and new VMs never exchange messages during a
  reload.
- Host objects (`http`, `request`, `server`, functions) do not cross a port.
  Only plain data, ports, `ArrayBuffer`, `Blob`, and `DOMException` do. Send a
  snapshot, not a live object.
- A host-shared `ArrayBuffer` cannot be transferred to a worker. Copy it.

The old background job package is deleted. A job is a worker, a message, or a
timer. Retry and deadline policy belong to the script or to Caddy, not to a
second scheduler.

### 4.9 Events

The app subscribes to Caddy events during provisioning via `ctx.App("events")`.
The events app accepts subscriptions only before it starts, and dispatches
synchronously on the emitting goroutine, so the hook only clones the event data
and enqueues it to the target VM mailbox. Worker VMs receive the event as a
message or an `EventTarget` dispatch. The global worker is the natural listener
for process-wide events.

The events app is experimental in Caddy; the subscription surface is isolated so
a change there does not leak into scripts.

## 5. Lifecycle

### 5.1 Provisioning

1. Parse and validate options.
2. Resolve the sandbox root and the entry module.
3. Create the runtime with memory, stack, locale, and time zone set.
4. Install only the configured capabilities.
5. Set the module loader to the sandbox.
6. Evaluate the entry module with a provision deadline. A syntax error or a
   rejected top-level await fails the config load.
7. Register the site VM in the generation registry.

Handles attach to the site VM through the registry; they do not create a second
runtime.

### 5.2 Reload

A Caddy reload provisions the new configuration before the old one stops.
Therefore:

- Every registry, worker, port, channel name, and watcher is scoped to one
  generation.
- Old and new VMs coexist briefly. A message must not cross generations.
- The old site VM is closed after its generation is retired. `Close` runs on the
  VM goroutine; Caddy's cleanup goroutine signals it through `AbortOn`.
- New VMs start with empty state. Configuration state is rebuilt from the files
  and the active config; request state is never migrated.

### 5.3 Backpressure and shutdown

- The admission queue is bounded per VM. HTTP requests over the limit receive
  `503`; worker work is rejected or dropped with a log entry.
- `queueMicrotask` and promise jobs are unbounded by the engine; the per-request
  event budget is enforced by the host around host callbacks.
- On shutdown, the VM stops accepting work, drains what the policy allows,
  closes ports and workers, cancels outstanding host work through the runtime
  context, and returns.

## 6. Security

- No filesystem, network, environment, timers, or module loader unless the host
  installs them. The VM starts with the language only.
- Module resolution is confined to the sandbox root. No `node_modules`, no npm,
  no CommonJS, no absolute paths outside the root, no symlink escape.
- `env_allow` is an allowlist, not a filter of a larger environment.
- `fetch_allow` sees every request, including redirects, and has a body limit.
- Do not install `Process`, `Run`, `Serve`, `Sockets`, or `DNS` for the target
  runtime. HTTP serving belongs to Caddy.
- Memory, stack, call depth, and per-call deadlines are set at construction.
- `eval` and `new Function` may be disabled with `WithoutCodeGeneration()`.
- Set the locale and time zone explicitly for deterministic output.
- The JIT is off. It would need a build tag, executable memory, and a separate
  memory budget.

## 7. Layout

- `javascript.go` at the repository root is the only Go file there. It blank
  imports the packages so their `init` registrations run.
- `pkg/config`: options, Caddyfile parsing, validation.
- `pkg/vm`: runtime construction, loop, generation registry, entry evaluation.
- `pkg/bridge`: `http`, request, response, headers, cookies, fetch, fs, timers.
- `pkg/template`: the `_` registry and typed conversion.
- `pkg/worker`: messaging, ports, events, worker construction.
- `pkg/handler`: the HTTP handler and the global app.
- Tests live next to their package. End-to-end assets live under `test/`.
- The shipped image config lives under `www/`.

## 8. Testing

- Unit tests cover option parsing, chain ordering, typed template conversion,
  structured clone and ports, generation scoping, and the mutable `server`
  draft.
- End-to-end tests use the image built by `./build.sh` and the harness in
  `test/`, with exact-byte comparisons. They cover:
  - state persists across requests in one site;
  - two sites do not share state;
  - a reload rebuilds VMs and drops state;
  - `route`, `respond`, `rewrite`, `next`, and `fallthrough` ordering;
  - `http.vars` visibility in downstream templates;
  - worker messages, ports, and teardown;
  - `server` draft mutation and `server.apply()` failure and success;
  - a missing or invalid script fails `caddy validate`.

Substring checks are not a substitute for exact bytes.

## 9. Non-goals and deferred

- Node compatibility, CommonJS, npm, and a vendored `node_modules`.
- Express and its middleware.
- A per-request VM.
- V8, cgo, and a C toolchain.
- The JIT, initially.
- VM pools for one site. One site is one thread; a pool would need a shared-note
  model and is a separate design.
- Streaming responses, SSE, and WebSocket upgrades. Add them after the core
  chain and lifecycle are stable.

## 10. Current state

The configuration surface, a per-directive go-quickjs VM, ESM evaluation with a
sandboxed loader, a minimal per-request `http` (`method`, `path`, `respond`),
timeouts, memory limits, queue admission, and VM cleanup on reload are
implemented. Site VM sharing, workers and messages, `server`, `_`, vars,
events, fetch, `fs`, and watching remain.
