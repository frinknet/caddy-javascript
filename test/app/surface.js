route("/surface/layer*", (http) => {
	http.headers["x-layer"] = "one"
})

export default function handler(http) {
	if (http.path === "/surface/signal") {
		http.respond(200, {}, [
			typeof http.signal,
			http.signal instanceof AbortSignal,
			http.signal.aborted,
		].join("|"))
		return
	}
	http.vars.echo = http.query.q ?? "-"
	http.respond(200, {
		"content-type": "text/plain",
		"x-layer": http.headers["x-layer"] ?? "none",
		"x-query": http.query.q ?? "-",
	}, [
		http.method,
		http.path,
		http.headers["x-layer"] ?? "none",
		http.query.q ?? "-",
		http.cookies.c ?? "-",
	].join("|"))
}
