package handler

import (
	"encoding/json"
	"fmt"
	"sort"
	"text/template"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// templateFuncs loads the functions declared by the site's `templates`
// extensions and returns them for the `_` registry. The templates handler is
// provisioned from the same route's raw JSON, so the extensions are still
// readable here even though Caddy has not finished loading the route.
func templateFuncs(ctx caddy.Context, route *caddyhttp.Route) (map[string]any, error) {
	return loadTemplateFuncs(route, ctx.LoadModuleByID)
}

type moduleLoader func(id string, raw json.RawMessage) (any, error)

func loadTemplateFuncs(route *caddyhttp.Route, load moduleLoader) (map[string]any, error) {
	configs := templateExtensionConfigs(route)
	if len(configs) == 0 {
		return nil, nil
	}

	names := make([]string, 0, len(configs))
	for name := range configs {
		names = append(names, name)
	}
	sort.Strings(names)

	funcs := make(map[string]any)
	for _, name := range names {
		moduleID := "http.handlers.templates.functions." + name
		module, err := load(moduleID, configs[name])
		if err != nil {
			return nil, fmt.Errorf("javascript: load template extension %q: %w", name, err)
		}
		provider, ok := module.(interface {
			CustomTemplateFunctions() template.FuncMap
		})
		if !ok {
			return nil, fmt.Errorf("javascript: template extension %q does not provide functions", name)
		}
		for fnName, fn := range provider.CustomTemplateFunctions() {
			funcs[fnName] = fn
		}
	}
	return funcs, nil
}

// templateExtensionConfigs collects the `extensions` module configs from every
// templates handler in the route tree. Caddy 2.11 stores them under the `match`
// JSON key because that is the tag on Templates.ExtensionsRaw; `extensions` is
// accepted too so the reader survives the tag being corrected.
func templateExtensionConfigs(route *caddyhttp.Route) map[string]json.RawMessage {
	if route == nil {
		return nil
	}
	configs := make(map[string]json.RawMessage)
	collectTemplateExtensions(route.HandlersRaw, configs)
	return configs
}

func collectTemplateExtensions(handlers []json.RawMessage, configs map[string]json.RawMessage) {
	for _, raw := range handlers {
		var handler struct {
			Handler    string                     `json:"handler"`
			Match      map[string]json.RawMessage `json:"match"`
			Extensions map[string]json.RawMessage `json:"extensions"`
			Routes     []json.RawMessage          `json:"routes"`
		}
		if json.Unmarshal(raw, &handler) != nil {
			continue
		}
		switch handler.Handler {
		case "templates":
			for name, config := range handler.Match {
				configs[name] = config
			}
			for name, config := range handler.Extensions {
				configs[name] = config
			}
		case "subroute":
			for _, rawRoute := range handler.Routes {
				var route struct {
					Handlers []json.RawMessage `json:"handle"`
				}
				if json.Unmarshal(rawRoute, &route) != nil {
					continue
				}
				collectTemplateExtensions(route.Handlers, configs)
			}
		}
	}
}
