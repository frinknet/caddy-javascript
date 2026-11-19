export default function handler(http) {
	if (http.query.token !== "secret") {
		http.respond(401, { "content-type": "text/plain" }, "unauthorized")
	}
}
