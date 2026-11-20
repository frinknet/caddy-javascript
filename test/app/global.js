import fs from "fs"

let calls = 0
let timer = null

events.on("started", (e) => {
	fs.writeFileSync("/events.txt", [e.name, e.origin, e.data === null ? "none" : "data"].join("|"))
})

watch("/watch", (names) => {
	fs.writeFileSync("/watch.txt", names.join(","))
})

function serverRoutes() {
	const config = globalThis.config
	if (!config || !config.apps || !config.apps.http) return []
	const servers = config.apps.http.servers || {}
	const lists = []
	for (const name of Object.keys(servers)) {
		if (Array.isArray(servers[name].routes)) lists.push(servers[name].routes)
	}
	return lists
}

function has(routes, path) {
	return routes.some((route) => JSON.stringify(route.match ?? []).includes(path))
}

function addRoute(lists, path, body) {
	for (const routes of lists) {
		if (!has(routes, path)) {
			routes.unshift({
				match: [{ path: [path] }],
				handle: [{ handler: "static_response", body: body }],
			})
		}
	}
}

function addState() {
	const lists = serverRoutes()
	if (lists.length === 0) return
	addRoute(lists, "/worker/state", "calls=" + calls)
	if (!globalThis.config.apply()) setTimeout(addState, 500)
}

timer = setInterval(() => {
	const lists = serverRoutes()
	if (lists.length === 0) return

	if (lists.every((routes) => has(routes, "/worker/config"))) {
		clearInterval(timer)
		setTimeout(addState, 1500)
		return
	}

	calls++
	addRoute(lists, "/worker/config", "worker")
	globalThis.config.apply()
}, 500)

export default {}
