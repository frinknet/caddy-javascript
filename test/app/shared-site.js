import { next } from "./shared-lib.js"

globalThis.shared = (globalThis.shared ?? 0) + 1

export default function handler(http) {
	http.respond(200, { "content-type": "text/plain" }, "site=" + globalThis.shared + " lib=" + next())
}
