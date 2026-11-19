import { label } from "./lib.js"

export default function handler(http) {
	http.respond(200, { "content-type": "text/plain" }, label(http.method, http.path))
}