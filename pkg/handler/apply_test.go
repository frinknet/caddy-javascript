package handler

import (
	"encoding/json"
	"testing"
)

func TestValidateCaddyConfigRequiresWorker(t *testing.T) {
	if err := validateCaddyConfig(json.RawMessage(`{"apps":{"http":{}}}`)); err == nil {
		t.Fatal("a config without the javascript app was accepted")
	}
	if err := validateCaddyConfig(json.RawMessage(`{"apps":{}}`)); err == nil {
		t.Fatal("a config without apps was accepted")
	}
	if err := validateCaddyConfig(json.RawMessage(`{"apps":{"javascript":{"script_path":"/a.js"}}}`)); err != nil {
		t.Fatalf("a config with the worker was rejected: %v", err)
	}
}

func TestAdminBaseUsesCADDYAdmin(t *testing.T) {
	t.Setenv("CADDY_ADMIN", "127.0.0.1:2020")
	if got := adminBase(); got != "http://127.0.0.1:2020" {
		t.Errorf("admin base = %q", got)
	}
	t.Setenv("CADDY_ADMIN", "https://admin.example")
	if got := adminBase(); got != "https://admin.example" {
		t.Errorf("admin base = %q", got)
	}
}
