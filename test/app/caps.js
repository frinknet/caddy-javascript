import fs from "fs"

let worker = "pending"
const child = new Worker("/worker.js", { type: "module" })
child.onmessage = (e) => { worker = "worker:" + e.data }

let channel = "pending"
const sender = new BroadcastChannel("caps")
const receiver = new BroadcastChannel("caps")
receiver.onmessage = (e) => { channel = "channel:" + e.data }
setTimeout(() => sender.postMessage("ok"), 5)

export default function handler(http) {
	switch (http.path) {
		case "/caps/fs":
			http.respond(200, {}, fs.readFileSync("/lib.js", "utf8").trim())
			return
		case "/caps/clone":
			http.respond(200, {}, JSON.stringify(structuredClone({ a: [1, 2], b: "x" })))
			return
		case "/caps/worker":
			http.respond(200, {}, worker)
			return
		case "/caps/channel":
			http.respond(200, {}, channel)
			return
		case "/caps/fetch":
			http.respond(200, {}, typeof fetch)
			return
		case "/caps/events":
			http.respond(200, {}, typeof globalThis.events)
			return
		default:
			http.respond(404, {}, "no")
	}
}
