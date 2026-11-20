export default function handler(http) {
	switch (http.path) {
		case "/template/env":
			http.respond(200, {}, _.env("TEMPLATE_TEST"))
			return
		case "/template/expandenv":
			http.respond(200, {}, _.expandenv("$TEMPLATE_TEST"))
			return
		case "/template/dns":
			http.respond(200, {}, _.getHostByName("127.0.0.1"))
			return
		case "/template/httpinclude":
			http.respond(200, {}, _.httpInclude("http://127.0.0.1/anything"))
			return
		default:
			http.respond(404, {}, "no")
	}
}
