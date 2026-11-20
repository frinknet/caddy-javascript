package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// generation tracks one configuration generation's handlers. Site VMs live in
// the process-wide pool instead, so a reload can reuse them.
type generation struct {
	mu       sync.Mutex
	handlers int
	refs     int
}

var generations sync.Map // *caddyhttp.Server -> *generation

func generationFor(srv *caddyhttp.Server) *generation {
	actual, _ := generations.LoadOrStore(srv, &generation{})
	return actual.(*generation)
}

// siteRef is one handler's attachment to the site VM pool.
type siteRef struct {
	srv      *caddyhttp.Server
	gen      *generation
	key      string
	identity string
	pool     string
	route    *caddyhttp.Route
}

func (g *generation) nextOrdinal() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	ordinal := g.handlers
	g.handlers++
	return ordinal
}

func (g *generation) hold() {
	g.mu.Lock()
	g.refs++
	g.mu.Unlock()
}

func (g *generation) drop(srv *caddyhttp.Server) {
	g.mu.Lock()
	g.refs--
	idle := g.refs == 0
	g.mu.Unlock()
	if idle {
		generations.CompareAndDelete(srv, g)
	}
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

// bindSite resolves the site identity for this handler. It reports false when
// there is no enclosing server, in which case the handler owns a private VM.
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

	var key, identity string
	route, index, found := handlerRoute(srv, ordinal)
	if h.Site != "" {
		key = "site:" + h.Site
		identity = key
	} else {
		if !found {
			return siteRef{}, false
		}
		if hostKey, ok := routeHostKey(route); ok {
			key = hostKey
			identity = hostKey
		} else {
			key = "route:" + strconv.Itoa(index)
			identity = routeIdentity(route, h.script)
		}
	}
	if !found {
		route = nil
	}

	return siteRef{srv: srv, gen: gen, key: key, identity: identity, route: route}, true
}

// routeIdentity keeps a hostless site's VM stable when a mount shifts its
// top-level route index.
func routeIdentity(route *caddyhttp.Route, script string) string {
	var matchers []byte
	if len(route.MatcherSetsRaw) > 0 {
		matchers, _ = json.Marshal(route.MatcherSetsRaw)
	} else if len(route.MatcherSets) > 0 {
		matchers, _ = json.Marshal(route.MatcherSets)
	}
	sum := sha256.Sum256(append(matchers, script...))
	return "route:" + hex.EncodeToString(sum[:])
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
