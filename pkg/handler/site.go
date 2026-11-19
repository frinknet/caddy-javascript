package handler

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"

	"github.com/frinknet/caddy-javascript/pkg/vm"
)

// generation is the provisioning state of one configuration generation. It is
// keyed by the server it belongs to, so an old and a new generation never
// share a site VM.
type generation struct {
	mu       sync.Mutex
	registry *vm.Registry
	handlers int
	refs     int
}

var generations sync.Map // *caddyhttp.Server -> *generation

func generationFor(srv *caddyhttp.Server) *generation {
	actual, _ := generations.LoadOrStore(srv, &generation{registry: vm.NewRegistry()})
	return actual.(*generation)
}

// siteRef is one handler's attachment to a site VM in a generation registry.
type siteRef struct {
	srv *caddyhttp.Server
	gen *generation
	key string
}

func (g *generation) nextOrdinal() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	ordinal := g.handlers
	g.handlers++
	return ordinal
}

func (g *generation) join(ctx context.Context, key string, cfg vm.Config) (*vm.VM, error) {
	v, err := g.registry.Join(ctx, key, cfg)
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	g.refs++
	g.mu.Unlock()
	return v, nil
}

func (g *generation) release(srv *caddyhttp.Server, key string) error {
	err := g.registry.Release(key)
	g.mu.Lock()
	g.refs--
	idle := g.refs == 0
	g.mu.Unlock()
	if idle {
		generations.CompareAndDelete(srv, g)
	}
	return err
}

// discard drops a generation that no handler attached to, such as after a
// failed join during configuration load.
func (g *generation) discard(srv *caddyhttp.Server) {
	g.mu.Lock()
	idle := g.refs == 0
	g.mu.Unlock()
	if idle {
		generations.CompareAndDelete(srv, g)
	}
}

// bindSite resolves the site key for this handler. It reports false when there
// is no enclosing server, in which case the handler owns a private VM.
func (h *Handler) bindSite(ctx caddy.Context) (siteRef, bool) {
	if ctx.Context == nil {
		return siteRef{}, false
	}
	srv, _ := ctx.Value(caddyhttp.ServerCtxKey).(*caddyhttp.Server)
	if srv == nil {
		return siteRef{}, false
	}

	gen := generationFor(srv)
	ordinal := gen.nextOrdinal()

	var key string
	if h.Site != "" {
		key = "site:" + h.Site
	} else {
		route, index, ok := handlerRoute(srv, ordinal)
		if !ok {
			return siteRef{}, false
		}
		if hostKey, ok := routeHostKey(route); ok {
			key = hostKey
		} else {
			key = "route:" + strconv.Itoa(index)
		}
	}

	return siteRef{srv: srv, gen: gen, key: key}, true
}

// handlerRoute returns the top-level route that contains the javascript
// handler at the given provisioning ordinal.
func handlerRoute(srv *caddyhttp.Server, ordinal int) (*caddyhttp.Route, int, bool) {
	seen := 0
	for i := range srv.Routes {
		route := &srv.Routes[i]
		count := countRouteHandlers(route)
		if ordinal < seen+count {
			return route, i, true
		}
		seen += count
	}
	return nil, -1, false
}

// countRouteHandlers counts the javascript handlers a route will provision.
// Routes that are fully provisioned keep them in Handlers; routes still being
// provisioned keep the raw JSON, so both shapes are counted.
func countRouteHandlers(route *caddyhttp.Route) int {
	if len(route.Handlers) > 0 || len(route.HandlersRaw) == 0 {
		return countLoadedHandlers(route.Handlers)
	}
	return countRawHandlers(route.HandlersRaw)
}

func countLoadedHandlers(handlers []caddyhttp.MiddlewareHandler) int {
	n := 0
	for _, handler := range handlers {
		switch h := handler.(type) {
		case *Handler:
			n++
		case *caddyhttp.Subroute:
			for i := range h.Routes {
				n += countRouteHandlers(&h.Routes[i])
			}
		}
	}
	return n
}

func countRawHandlers(raw []json.RawMessage) int {
	type handlerJSON struct {
		Handler string            `json:"handler"`
		Routes  []json.RawMessage `json:"routes"`
	}
	type routeJSON struct {
		Handlers []json.RawMessage `json:"handle"`
	}

	n := 0
	for _, msg := range raw {
		var h handlerJSON
		if json.Unmarshal(msg, &h) != nil {
			continue
		}
		switch h.Handler {
		case "javascript":
			n++
		case "subroute":
			for _, routeMsg := range h.Routes {
				var r routeJSON
				if json.Unmarshal(routeMsg, &r) != nil {
					continue
				}
				n += countRawHandlers(r.Handlers)
			}
		}
	}
	return n
}

// routeHostKey builds a stable site key from a route's host matchers. Other
// matchers, such as path, do not identify a site.
func routeHostKey(route *caddyhttp.Route) (string, bool) {
	seen := make(map[string]struct{})
	var hosts []string
	add := func(values []string) {
		for _, host := range values {
			if _, dup := seen[host]; dup {
				continue
			}
			seen[host] = struct{}{}
			hosts = append(hosts, host)
		}
	}

	// Caddy provisions matchers before handlers, so the loaded form is the
	// one present when a handler provisions.
	for _, set := range route.MatcherSets {
		for _, matcher := range set {
			switch m := matcher.(type) {
			case caddyhttp.MatchHost:
				add(m)
			case *caddyhttp.MatchHost:
				add(*m)
			}
		}
	}

	if len(hosts) == 0 {
		for _, set := range route.MatcherSetsRaw {
			raw, ok := set["host"]
			if !ok {
				continue
			}
			var values []string
			if json.Unmarshal(raw, &values) != nil {
				continue
			}
			add(values)
		}
	}

	if len(hosts) == 0 {
		return "", false
	}
	sort.Strings(hosts)
	return "host:" + strings.Join(hosts, ","), true
}
