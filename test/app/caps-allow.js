let result = "pending"
const attempt = () => {
	fetch("http://127.0.0.1/anything")
		.then((r) => r.text())
		.then((t) => { result = t })
		.catch(() => setTimeout(attempt, 100))
}
attempt()

let denied = "pending"
fetch("http://localhost/anything")
	.then(() => { denied = "allowed" })
	.catch(() => { denied = "denied" })

export default function handler(http) {
	switch (http.path) {
		case "/caps/fetch":
			http.respond(200, {}, result)
			return
		case "/caps/fetch-denied":
			http.respond(200, {}, denied)
			return
		default:
			http.respond(404, {}, "no")
	}
}
