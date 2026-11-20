let got = "none"
const channel = new BroadcastChannel("probe")
channel.onmessage = (e) => { got = "got:" + e.data }

export default function handler(http) {
	http.respond(200, {}, got)
}
