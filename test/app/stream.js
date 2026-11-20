export default function handler(http) {
	switch (http.path) {
	case "/stream/sync": {
		const out = http.stream(200, { "content-type": "text/plain" })
		out.write("a|")
		out.write("b|")
		out.end("c")
		return
	}
	case "/stream/async": {
		const out = http.stream(200, { "content-type": "text/plain" })
		out.write("first|")
		setTimeout(() => out.end("second"), 20)
		return
	}
	case "/stream/sse": {
		const sse = http.sse()
		sse.send("one")
		sse.send("two", { event: "tick", id: "1" })
		sse.close()
		return
	}
	case "/stream/ws": {
		const ws = http.websocket()
		ws.send("hello")
		ws.close(1000)
		return
	}
	}
	http.respond(404, {}, "not found")
}
