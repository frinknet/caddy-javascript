export default function handler(http) {
	http.respond(200, { "content-type": "text/plain" }, "token=" + http.query.token)
}
