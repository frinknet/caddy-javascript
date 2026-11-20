export default function handler(http) {
	if (http.path === "/template/env") {
		_.env("PATH")
	}
	const upper = _.upper("hello")
	const escaped = _.pathEscape("a b")
	http.respond(200, { "content-type": "text/plain" }, upper + "|" + escaped)
}
