const channel = new BroadcastChannel("probe")
setInterval(() => channel.postMessage("leak"), 100)

export default function handler(http) {
	http.respond(200, {}, "a")
}
