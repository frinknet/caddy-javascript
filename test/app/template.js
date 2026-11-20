export default function handler(http) {
	switch (http.path) {
		case "/template/upper":
			http.respond(200, {}, _.upper("hello") + "|" + _.pathEscape("a b"))
			return
		case "/template/placeholder":
			http.respond(200, {}, _.placeholder("host"))
			return
		case "/template/read":
			http.respond(200, {}, _.readFile("lib.js"))
			return
		case "/template/exists":
			http.respond(200, {}, String(_.fileExists("lib.js")) + "|" + String(_.fileExists("missing.js")))
			return
		case "/template/list":
			http.respond(200, {}, _.listFiles(".").includes("lib.js") ? "yes" : "no")
			return
		case "/template/stat":
			const stat = _.fileStat("lib.js")
			http.respond(200, {}, stat.Name + "|" + String(stat.Size) + "|" + String(stat.IsDir))
			return
		case "/template/humanize":
			http.respond(200, {}, _.humanize("size", "2048000"))
			return
		case "/template/frontmatter":
			const doc = _.splitFrontMatter("---\ntitle: Hi\n---\nBody")
			http.respond(200, {}, doc.Meta.title + "|" + doc.Body.trim())
			return
		case "/template/html":
			http.respond(200, {}, _.stripHTML("<p>hi</p>"))
			return
		case "/template/markdown":
			http.respond(200, {}, _.markdown("# hi"))
			return
		case "/template/status":
			http.respond(200, {}, _.httpStatus(404))
			return
		case "/template/error":
			_.httpError(404)
			return
		case "/template/epoch":
			http.respond(200, {}, _.unixEpoch(new Date("2020-01-02T00:00:00Z")))
			return
		case "/template/date":
			http.respond(200, {}, _.toDate("2006-01-02", "2020-01-02"))
			return
		case "/template/include":
			http.respond(200, {}, _.include("inc.html"))
			return
		case "/template/env":
			_.env("PATH")
			return
		case "/template/expandenv":
			_.expandenv("$PATH")
			return
		case "/template/dns":
			_.getHostByName("localhost")
			return
		case "/template/httpinclude":
			_.httpInclude("http://127.0.0.1/anything")
			return
		case "/template/escape":
			_.readFile("../etc/passwd")
			return
		default:
			http.respond(404, {}, "no")
	}
}
