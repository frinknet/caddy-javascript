let result = "pending"
const attempt = () => {
	fetch("http://127.0.0.1/anything")
		.then((r) => r.text())
		.then((t) => { result = t })
		.catch(() => setTimeout(attempt, 100))
}
attempt()

export default function handler(http) {
	http.respond(200, {}, result)
}
