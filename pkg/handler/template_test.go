package handler

import (
	"encoding/json"
	"errors"
	"testing"
	"text/template"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

func TestTemplateExtensionConfigs(t *testing.T) {
	route := &caddyhttp.Route{HandlersRaw: []json.RawMessage{
		json.RawMessage(`{"handler":"templates","match":{"site_ext":{"value":"top"}}}`),
		json.RawMessage(`{"handler":"javascript"}`),
		json.RawMessage(`{"handler":"subroute","routes":[{"handle":[{"handler":"templates","match":{"nested_ext":{"value":"nested"}}}]}]}`),
	}}

	configs := templateExtensionConfigs(route)
	if len(configs) != 2 {
		t.Fatalf("configs = %v, want two extensions", configs)
	}
	for name, want := range map[string]string{
		"site_ext":   `{"value":"top"}`,
		"nested_ext": `{"value":"nested"}`,
	} {
		if string(configs[name]) != want {
			t.Errorf("config %q = %s, want %s", name, configs[name], want)
		}
	}
}

func TestTemplateExtensionConfigsWithoutTemplates(t *testing.T) {
	route := &caddyhttp.Route{HandlersRaw: []json.RawMessage{
		json.RawMessage(`{"handler":"javascript"}`),
	}}
	if configs := templateExtensionConfigs(route); len(configs) != 0 {
		t.Fatalf("configs = %v, want none", configs)
	}
	if configs := templateExtensionConfigs(nil); len(configs) != 0 {
		t.Fatalf("nil route configs = %v, want none", configs)
	}
}

type fakeFunctions struct {
	funcs template.FuncMap
}

func (f fakeFunctions) CustomTemplateFunctions() template.FuncMap { return f.funcs }

func TestLoadTemplateFuncs(t *testing.T) {
	route := &caddyhttp.Route{HandlersRaw: []json.RawMessage{
		json.RawMessage(`{"handler":"templates","match":{"site_ext":{"value":"top"}}}`),
	}}

	var loadedID string
	funcs, err := loadTemplateFuncs(route, func(id string, raw json.RawMessage) (any, error) {
		loadedID = id
		if string(raw) != `{"value":"top"}` {
			t.Fatalf("raw config = %s", raw)
		}
		return fakeFunctions{funcs: template.FuncMap{
			"shout": func(s string) string { return s + "!" },
		}}, nil
	})
	if err != nil {
		t.Fatalf("loadTemplateFuncs: %v", err)
	}
	if loadedID != "http.handlers.templates.functions.site_ext" {
		t.Fatalf("module id = %q", loadedID)
	}
	if _, ok := funcs["shout"]; !ok {
		t.Fatalf("funcs = %v, want shout", funcs)
	}
}

func TestLoadTemplateFuncsErrors(t *testing.T) {
	route := &caddyhttp.Route{HandlersRaw: []json.RawMessage{
		json.RawMessage(`{"handler":"templates","match":{"site_ext":{}}}`),
	}}

	if _, err := loadTemplateFuncs(route, func(string, json.RawMessage) (any, error) {
		return nil, errors.New("boom")
	}); err == nil {
		t.Fatal("loadTemplateFuncs accepted a failing module")
	}

	if _, err := loadTemplateFuncs(route, func(string, json.RawMessage) (any, error) {
		return struct{}{}, nil
	}); err == nil {
		t.Fatal("loadTemplateFuncs accepted a module without functions")
	}
}
