package bridge

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

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
	fullPath := p.staticReqString(req, "path")
	rest, ok := router.Rest("", fullPath)
	if !ok {
		next()
		return
	}
	p.serveStaticFile(cfg, rest, fullPath, strings.ToUpper(p.staticReqString(req, "method")), p.staticHeaders(req), next)
}

// serveStaticFile resolves and serves one file below cfg.Root. Work runs off
// the event loop; next resumes the chain on it.
func (p *Program) serveStaticFile(cfg *router.Static, rest, fullPath, method string, headers http.Header, next func()) {
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
		file, info, finalName, isDir, ok := resolveStatic(p.fs, name, cfg)
		if ok && isDir && !strings.HasSuffix(rest, "/") {
			if file != nil {
				_ = file.Close()
			}
			p.submitAsync(ex, func() {
				w := ex.Writer()
				if w.Written() {
					return
				}
				w.Header().Set("Location", fullPath+"/")
				w.WriteHeader(http.StatusMovedPermanently)
			})
			return
		}
		served := false
		if ok && file != nil {
			p.serveStaticContent(ex, cfg, file, info, finalName, method, fullPath, headers)
			served = true
		}
		p.submitAsync(ex, func() {
			if served {
				return
			}
			if cfg.Fallthrough {
				next()
				return
			}
			if !ex.Writer().Written() {
				http.Error(ex.Writer(), http.StatusText(http.StatusNotFound), http.StatusNotFound)
			}
		})
	}()
}

func (p *Program) serveStaticContent(ex *Exchange, cfg *router.Static, file *os.File, info os.FileInfo, name, method, fullPath string, headers http.Header) {
	defer file.Close()
	w := ex.Writer()
	if w.Written() {
		return
	}
	w.Header().Set("Accept-Ranges", "bytes")
	if cfg.MaxAgeSet {
		maxAge := int64(cfg.MaxAge / time.Second)
		if maxAge < 0 {
			maxAge = 0
		}
		w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", maxAge))
	}
	if cfg.ETag {
		w.Header().Set("ETag", staticETag(info))
	} else {
		w.Header().Del("ETag")
	}
	req := &http.Request{
		Method: method,
		Header: headers.Clone(),
		URL:    &url.URL{Path: fullPath},
	}
	http.ServeContent(w, req, name, info.ModTime(), file)
}

// resolveStatic finds the file to serve. It reports isDir when the request
// resolved to a directory, so the caller can redirect to a trailing slash.
// file is nil when the directory has no index.
func resolveStatic(fsys *files.FS, name string, cfg *router.Static) (*os.File, os.FileInfo, string, bool, bool) {
	info, err := fsys.Stat(name)
	if err != nil {
		found := false
		for _, ext := range cfg.Extensions {
			candidate := name + "." + ext
			candidateInfo, candidateErr := fsys.Stat(candidate)
			if candidateErr == nil && !candidateInfo.IsDir() {
				name, info, err = candidate, candidateInfo, nil
				found = true
				break
			}
		}
		if !found {
			return nil, nil, "", false, false
		}
	}
	if info.IsDir() {
		if cfg.Index == "" {
			return nil, info, name, true, true
		}
		indexName := path.Join(name, cfg.Index)
		indexInfo, indexErr := fsys.Stat(indexName)
		if indexErr != nil || indexInfo.IsDir() {
			return nil, info, name, true, true
		}
		file, openErr := fsys.Open(indexName)
		if openErr != nil {
			return nil, nil, "", false, false
		}
		return file, indexInfo, indexName, true, true
	}
	file, openErr := fsys.Open(name)
	if openErr != nil {
		return nil, nil, "", false, false
	}
	return file, info, name, false, true
}

func (p *Program) staticHeaders(req sobek.Value) http.Header {
	headers := make(http.Header)
	if isNilValue(req) {
		return headers
	}
	obj := req.ToObject(p.rt)
	if obj == nil {
		return headers
	}
	hv := obj.Get("headers")
	if isNilValue(hv) {
		return headers
	}
	hobj := hv.ToObject(p.rt)
	if hobj == nil {
		return headers
	}
	for _, name := range hobj.Keys() {
		value := hobj.Get(name)
		if !isNilValue(value) {
			headers.Set(name, value.String())
		}
	}
	return headers
}

func staticETag(info os.FileInfo) string {
	return fmt.Sprintf(`"%x-%x"`, info.Size(), info.ModTime().UnixNano())
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
