package vm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHTTPSignalIsAbortSignal(t *testing.T) {
	v, entry := streamVM(t, `export default function handler(http) {
	http.respond(200, {}, [typeof http.signal, http.signal instanceof AbortSignal, http.signal.aborted].join("|"))
}
`)
	defer v.Close()

	resp := handle(t, v, entry, http.MethodGet, "/")
	if resp == nil || string(resp.Body) != "object|true|false" {
		t.Fatalf("response = %+v", resp)
	}
}

func TestHTTPSignalAbortsWhenRequestEnds(t *testing.T) {
	v, entry := streamVM(t, `let aborted = "no"
export default function handler(http) {
	if (aborted === "no") http.signal.addEventListener("abort", () => { aborted = "yes" })
	http.respond(200, {}, aborted)
}
`)
	defer v.Close()

	ctx, cancel := context.WithCancel(context.Background())
	resp, err := v.Handle(ctx, entry, testRequest(http.MethodGet, "/", nil))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if resp == nil || string(resp.Body) != "no" {
		t.Fatalf("response = %+v", resp)
	}
	cancel()
	if got := waitBody(t, v, entry, func(body string) bool { return body == "yes" }); got != "yes" {
		t.Errorf("signal state = %q, want %q", got, "yes")
	}
}

func TestFetchInheritsRequestSignal(t *testing.T) {
	received := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case received <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	dir := t.TempDir()
	entry := writeModule(t, dir, "main.js", `let started = false
let status = "pending"
export default function handler(http) {
	if (!started) {
		started = true
		fetch("`+srv.URL+`").then(() => { status = "ok" }).catch(() => { status = "aborted" })
	}
	http.respond(200, {}, status)
}
`)
	v, err := New(Config{
		ScriptPath:     entry,
		RootDir:        dir,
		RequireHandler: true,
		FetchAllow:     []string{"127.0.0.1"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer v.Close()

	ctx, cancel := context.WithCancel(context.Background())
	resp, err := v.Handle(ctx, entry, testRequest(http.MethodGet, "/", nil))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if resp == nil || string(resp.Body) != "pending" {
		t.Fatalf("response = %+v", resp)
	}

	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("fetch never reached the test server")
	}

	cancel()
	if got := waitBody(t, v, entry, func(body string) bool { return body != "pending" }); got != "aborted" {
		t.Errorf("fetch outcome = %q, want %q", got, "aborted")
	}
}

func TestFetchInheritsSignalAcrossAwait(t *testing.T) {
	received := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case received <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	dir := t.TempDir()
	entry := writeModule(t, dir, "main.js", `let started = false
let status = "pending"
async function begin() {
	await Promise.resolve()
	fetch("`+srv.URL+`").then(() => { status = "ok" }).catch(() => { status = "aborted" })
}
export default function handler(http) {
	if (!started) {
		started = true
		begin()
	}
	http.respond(200, {}, status)
}
`)
	v, err := New(Config{
		ScriptPath:     entry,
		RootDir:        dir,
		RequireHandler: true,
		FetchAllow:     []string{"127.0.0.1"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer v.Close()

	ctx, cancel := context.WithCancel(context.Background())
	resp, err := v.Handle(ctx, entry, testRequest(http.MethodGet, "/", nil))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if resp == nil || string(resp.Body) != "pending" {
		t.Fatalf("response = %+v", resp)
	}

	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("fetch after await never reached the test server")
	}

	cancel()
	if got := waitBody(t, v, entry, func(body string) bool { return body != "pending" }); got != "aborted" {
		t.Errorf("fetch outcome = %q, want %q", got, "aborted")
	}
}
