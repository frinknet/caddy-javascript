package config

import (
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
)

func parseTest(t *testing.T, input string) (Options, error) {
	t.Helper()
	d := caddyfile.NewTestDispenser(input)
	return Parse(httpcaddyfile.Helper{Dispenser: d})
}

func TestParseInlinePath(t *testing.T) {
	opts, err := parseTest(t, "javascript /app/server.js")
	if err != nil {
		t.Fatal(err)
	}
	if opts.ScriptPath != "/app/server.js" {
		t.Fatalf("script path = %q", opts.ScriptPath)
	}
}

func TestParseBlockOptions(t *testing.T) {
	opts, err := parseTest(t, `javascript /app/server.js {
	timeout 5s
	queue_size 10
	max_body 2048
	fallthrough
	env_allow FOO BAR
	fetch_allow example.com
}`)
	if err != nil {
		t.Fatal(err)
	}
	if opts.ScriptPath != "/app/server.js" {
		t.Fatalf("script path = %q", opts.ScriptPath)
	}
	if opts.Timeout != caddy.Duration(5*time.Second) {
		t.Fatalf("timeout = %v", time.Duration(opts.Timeout))
	}
	if opts.QueueSize != 10 {
		t.Fatalf("queue size = %d", opts.QueueSize)
	}
	if opts.MaxBody != 2048 {
		t.Fatalf("max body = %d", opts.MaxBody)
	}
	if !opts.Fallthrough {
		t.Fatal("fallthrough not set")
	}
	if len(opts.EnvAllow) != 2 || opts.EnvAllow[0] != "FOO" {
		t.Fatalf("env allow = %v", opts.EnvAllow)
	}
	if len(opts.FetchAllow) != 1 || opts.FetchAllow[0] != "example.com" {
		t.Fatalf("fetch allow = %v", opts.FetchAllow)
	}
}

func TestParseMissingPath(t *testing.T) {
	if _, err := parseTest(t, "javascript"); err == nil {
		t.Fatal("expected an error for a missing script path")
	}
}
