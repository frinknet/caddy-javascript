import { next } from "./shared-lib.js"

export default function handler(http) {
	http.respond(200, { "content-type": "text/plain" }, "api=" + globalThis.shared + " lib=" + next())
}
