# caddy-javascript

Run ESM JavaScript inside Caddy as a native runtime. The target design is in
[`spec.md`](spec.md).

**Status: first runtime slice.** A site directive now creates a real
[go-quickjs](https://github.com/go-quickjs/go-quickjs) VM, evaluates the entry
module once, and dispatches each request to its default export. The global
option evaluates a worker entry but does not dispatch requests. Workers,
messaging, `server`, `_`, vars, events, fetch, and filesystem access are not
implemented yet.

## What works today

- The `javascript` global option provisions a VM and evaluates its entry module.
- The `javascript` HTTP directive provisions a site VM. The entry module must
  export a default function: `export default function handler(http) { ... }`.
- ESM imports resolve inside `root`. Bare specifiers, npm, CommonJS, and paths
  outside the root are rejected.
- Each request gets a fresh `http` object with `method`, `path`, and
  `respond(status, headers, body)`.
- `respond` writes the status, headers, and body. A handler that returns without
  responding answers `404`, or calls the next Caddy handler when `fallthrough`
  is set.
- `timeout` interrupts a runaway handler and answers `504`. A full per-VM queue
  answers `503`. A thrown exception answers `500`.
- `memory` and `queue_size` are active. A reload closes the old VMs.
- Each `javascript` directive owns one VM for now. Sharing one VM across a
  site's handle directives is the next step.

Parsed but not active yet: `site`, `env_allow`, `fetch_allow`, and `watch`.
`root`, `timeout`, `memory`, `queue_size`, and `fallthrough` are active.

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
| `site <name>` | Explicit site key. Parsed, not active yet. | discovered from the route |
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

export default function handler(http) {
	http.respond(200, { "content-type": "text/plain" }, label(http.method, http.path))
}
```

`http` currently has `method`, `path`, and `respond`. Timers are installed.
`server`, `_`, `http.vars`, `fetch`, `fs`, workers, `MessageChannel`, and Caddy
events are not available yet.

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
- `pkg/vm` holds the go-quickjs runtime, sandbox loader, event loop, and
  per-request dispatch.
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