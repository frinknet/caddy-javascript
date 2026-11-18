package config_test

import (
	"testing"

	"github.com/caddyserver/caddy/v2/caddyconfig"
	_ "github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"

	_ "github.com/frinknet/caddy-javascript"
)

func TestAdaptCaddyfile(t *testing.T) {
	adapter := caddyconfig.GetAdapter("caddyfile")
	if adapter == nil {
		t.Fatal("caddyfile adapter is not registered")
	}

	_, warnings, err := adapter.Adapt([]byte(":8080 {\n\tjavascript /e2e/app/server.js\n}\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range warnings {
		t.Logf("warning: %v", w)
	}
}
