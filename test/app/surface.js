route("/surface/layer*", (http, next) => {
	http.headers.set("x-layer", "one")
	next()
})

export default function handler(http) {
	http.vars.echo = http.query.q ?? "-"
	http.respond(200, {
		"content-type": "text/plain",
		"x-layer": http.headers.get("x-layer") ?? "none",
		"x-query": http.query.q ?? "-",
	}, [
		http.method,
		http.path,
		http.headers.get("x-layer") ?? "none",
		http.query.q ?? "-",
		http.cookies.c ?? "-",
	].join("|"))
}
