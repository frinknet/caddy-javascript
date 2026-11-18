package bridge

import (
	"mime"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/grafana/sobek"

	"github.com/frinknet/caddy-javascript/pkg/files"
	"github.com/frinknet/caddy-javascript/pkg/router"
)

type dotAction int

const (
	dotAllow dotAction = iota
	dotIgnore
	dotDeny
)

// serveStaticJS serves a static middleware that was called directly instead of
// being mounted on app.use. There is no mount path to strip.
func (p *Program) serveStaticJS(cfg *router.Static, call sobek.FunctionCall) {
	req := call.Argument(0)
	nextValue := call.Argument(2)
	next := func() {
		if fn, ok := sobek.AssertFunction(nextValue); ok {
			_, _ = fn(sobek.Undefined())
		}
	}
	rest, ok := router.Rest("", p.staticReqString(req, "path"))
	if !ok {
		next()
		return
	}
	p.serveStaticFile(cfg, rest, strings.ToUpper(p.staticReqString(req, "method")), next)
}

// serveStaticFile resolves and serves one file below cfg.Root. Work runs off
// the event loop; next resumes the chain on it.
func (p *Program) serveStaticFile(cfg *router.Static, rest, method string, next func()) {
	if method != "GET" && method != "HEAD" {
		next()
		return
	}

	action := dotfileAction(rest, cfg.Dotfiles)
	if action == dotDeny {
		ex := p.current
		if ex == nil {
			next()
			return
		}
		if !ex.Writer().Written() {
			http.Error(ex.Writer(), http.StatusText(http.StatusForbidden), http.StatusForbidden)
		}
		return
	}
	if action == dotIgnore {
		next()
		return
	}

	ex := p.trackAsync()
	if ex == nil {
		next()
		return
	}

	name := path.Join("/", cfg.Root, rest)
	go func() {
		data, info, finalName, ok := readStatic(p.fs, name, cfg)
		p.submitAsync(ex, func() {
			if !ok {
				if cfg.Fallthrough {
					next()
					return
				}
				if !ex.Writer().Written() {
					http.Error(ex.Writer(), http.StatusText(http.StatusNotFound), http.StatusNotFound)
				}
				return
			}
			w := ex.Writer()
			if w.Written() {
				return
			}
			w.Header().Set("Content-Type", contentTypeFor(finalName))
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			if !info.ModTime().IsZero() {
				w.Header().Set("Last-Modified", info.ModTime().UTC().Format(http.TimeFormat))
			}
			w.WriteHeader(http.StatusOK)
			if method != "HEAD" {
				_, _ = w.Write(data)
			}
		})
	}()
}

func readStatic(fsys *files.FS, name string, cfg *router.Static) ([]byte, os.FileInfo, string, bool) {
	info, err := fsys.Stat(name)
	if err != nil {
		return nil, nil, "", false
	}
	if info.IsDir() {
		if cfg.Index == "" {
			return nil, nil, "", false
		}
		name = path.Join(name, cfg.Index)
		info, err = fsys.Stat(name)
		if err != nil || info.IsDir() {
			return nil, nil, "", false
		}
	}
	data, err := fsys.ReadFile(name)
	if err != nil {
		return nil, nil, "", false
	}
	return data, info, name, true
}

func dotfileAction(rest, mode string) dotAction {
	hasDot := false
	for _, segment := range strings.Split(rest, "/") {
		if len(segment) > 1 && segment[0] == '.' {
			hasDot = true
			break
		}
	}
	if !hasDot {
		return dotAllow
	}
	switch strings.ToLower(mode) {
	case "allow":
		return dotAllow
	case "deny":
		return dotDeny
	default:
		return dotIgnore
	}
}

func contentTypeFor(name string) string {
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

func (p *Program) staticReqString(req sobek.Value, name string) string {
	if isNilValue(req) {
		return ""
	}
	obj := req.ToObject(p.rt)
	if obj == nil {
		return ""
	}
	v := obj.Get(name)
	if isNilValue(v) {
		return ""
	}
	return v.String()
}
