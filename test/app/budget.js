export default function handler(http) {
	for (let i = 0; i < 10001; i++) http.headers["x-probe"]
	http.respond(200, {}, "unreachable")
}
