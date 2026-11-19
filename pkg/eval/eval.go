package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/caddyserver/caddy/v2"
)

// Target is implemented by a javascript directive that opted into eval. The
// admin endpoint only calls targets that registered themselves.
type Target interface {
	EvalCode(ctx context.Context, code string) (string, error)
}

var targets sync.Map

// Register makes a target reachable through the admin eval endpoint.
func Register(name string, t Target) {
	targets.Store(name, t)
}

// Unregister removes a target. It is safe to call for an unknown name.
func Unregister(name string) {
	targets.Delete(name)
}

// Lookup returns a registered target.
func Lookup(name string) (Target, bool) {
	v, ok := targets.Load(name)
	if !ok {
		return nil, false
	}
	t, ok := v.(Target)
	return t, ok
}

func init() {
	caddy.RegisterModule(Admin{})
}

// Admin exposes the gated eval endpoint on the Caddy admin API. It is only
// reachable when a javascript directive opts in with `eval`.
type Admin struct{}

func (Admin) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "admin.api.javascript",
		New: func() caddy.Module { return new(Admin) },
	}
}

func (Admin) Provision(caddy.Context) error { return nil }

func (Admin) Routes() []caddy.AdminRoute {
	return []caddy.AdminRoute{
		{
			Pattern: "/javascript/eval",
			Handler: caddy.AdminHandlerFunc(handleEval),
		},
	}
}

func handleEval(w http.ResponseWriter, r *http.Request) error {
	if r.Method != http.MethodPost {
		return caddy.APIError{HTTPStatus: http.StatusMethodNotAllowed, Err: fmt.Errorf("method not allowed")}
	}
	var req struct {
		Target string `json:"target"`
		Code   string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return caddy.APIError{HTTPStatus: http.StatusBadRequest, Err: err}
	}
	t, ok := Lookup(req.Target)
	if !ok {
		return caddy.APIError{HTTPStatus: http.StatusNotFound, Err: fmt.Errorf("unknown eval target %q", req.Target)}
	}
	out, err := t.EvalCode(r.Context(), req.Code)
	if err != nil {
		return caddy.APIError{HTTPStatus: http.StatusBadRequest, Err: err}
	}
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(map[string]string{"result": out})
}
