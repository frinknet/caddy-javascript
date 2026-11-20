package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"

	"github.com/frinknet/caddy-javascript/pkg/config"
)

func reloadServer(script string) *caddyhttp.Server {
	return &caddyhttp.Server{
		Listen: []string{":80"},
		Routes: caddyhttp.RouteList{{
			MatcherSetsRaw: caddyhttp.RawMatcherSets{caddy.ModuleMap{
				"host": json.RawMessage(`["example.com"]`),
			}},
			Handlers: []caddyhttp.MiddlewareHandler{},
			HandlersRaw: []json.RawMessage{
				json.RawMessage(fmt.Sprintf(`{"handler":"javascript","script_path":%q}`, script)),
			},
		}},
	}
}

func TestEditedModuleReplacesTheVM(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, "site.js", `export default function handler(http) {
	http.respond(200, {}, "one")
}
`)
	srv := reloadServer(script)
	first := &Handler{Options: config.Options{ScriptPath: script}}
	if err := first.Provision(serverContext(srv)); err != nil {
		t.Fatalf("Provision first: %v", err)
	}
	defer first.Cleanup()
	if got := responseFor(t, first, http.MethodGet, "/"); got != "one" {
		t.Fatalf("first body = %q", got)
	}

	writeScript(t, dir, "site.js", `export default function handler(http) {
	http.respond(200, {}, "two")
}
`)
	second := &Handler{Options: config.Options{ScriptPath: script}}
	if err := second.Provision(serverContext(reloadServer(script))); err != nil {
		t.Fatalf("Provision after edit: %v", err)
	}
	defer second.Cleanup()

	if first.vm == second.vm {
		t.Fatal("an edited module reused the old VM")
	}
	if got := responseFor(t, second, http.MethodGet, "/"); got != "two" {
		t.Errorf("new generation body = %q, want %q", got, "two")
	}
	if got := responseFor(t, first, http.MethodGet, "/"); got != "one" {
		t.Errorf("old generation body = %q, want its running module %q", got, "one")
	}
}

func TestConcurrentRequestsDuringReload(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, "site.js", `let hits = 0
export default function handler(http) {
	hits++
	http.respond(200, {}, http.path + ":" + hits)
}
`)
	srv := reloadServer(script)
	first := &Handler{Options: config.Options{ScriptPath: script}}
	if err := first.Provision(serverContext(srv)); err != nil {
		t.Fatalf("Provision first: %v", err)
	}
	defer first.Cleanup()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodGet, "/", nil)
				err := first.ServeHTTP(rec, req, caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error {
					return nil
				}))
				if err != nil {
					errs <- err
					return
				}
				if rec.Code != http.StatusOK {
					errs <- fmt.Errorf("status = %d", rec.Code)
					return
				}
			}
		}()
	}

	second := &Handler{Options: config.Options{ScriptPath: script}}
	if err := second.Provision(serverContext(reloadServer(script))); err != nil {
		t.Fatalf("Provision second: %v", err)
	}
	if first.vm != second.vm {
		t.Fatal("an unchanged reload replaced the VM")
	}
	if err := second.Cleanup(); err != nil {
		t.Fatalf("cleanup second: %v", err)
	}
	close(stop)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("request during reload: %v", err)
	}
}
