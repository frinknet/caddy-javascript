package template

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"text/template"

	"github.com/Masterminds/sprig/v3"
	"github.com/yuin/goldmark"
)

var deniedSprig = map[string]bool{
	"env":           true,
	"expandenv":     true,
	"getHostByName": true,
}

// AddSprig registers the sprig function map minus the functions denied by the
// security denylist. The denied functions are re-added with capability checks
// by AddCaddyBuiltins.
func (r *Registry) AddSprig() error {
	for name, fn := range sprig.TxtFuncMap() {
		if deniedSprig[name] {
			continue
		}
		if err := r.Add(name, fn, false); err != nil {
			return err
		}
	}
	return nil
}

// AddCaddyBuiltins registers the mirrored Caddy template functions. The
// functions that need a request are marked request-bound.
func (r *Registry) AddCaddyBuiltins(host Host) error {
	b := &builtins{host: host, registry: r}
	functions := []struct {
		name         string
		fn           any
		requestBound bool
	}{
		{"placeholder", b.placeholder, true},
		{"ph", b.placeholder, true},
		{"env", b.env, false},
		{"expandenv", b.expandenv, false},
		{"getHostByName", b.getHostByName, false},
		{"readFile", b.readFile, true},
		{"fileExists", b.fileExists, true},
		{"listFiles", b.listFiles, true},
		{"stripHTML", b.stripHTML, false},
		{"markdown", b.markdown, false},
		{"pathEscape", url.PathEscape, false},
		{"httpError", b.httpError, true},
		{"httpStatus", b.httpStatus, false},
		{"httpInclude", b.httpInclude, true},
		{"include", b.include, true},
	}
	for _, f := range functions {
		if err := r.Add(f.name, f.fn, f.requestBound); err != nil {
			return err
		}
	}
	return nil
}

type builtins struct {
	host     Host
	registry *Registry
}

func (b *builtins) placeholder(name string) (any, error) {
	req := b.host.Request()
	if req == nil {
		return nil, errors.New("javascript: placeholder requires a request")
	}
	value, ok := b.host.Placeholder(name)
	if !ok {
		return nil, nil
	}
	return value, nil
}

func (b *builtins) env(name string) (string, error) {
	if !b.host.EnvAllowed(name) {
		return "", fmt.Errorf("javascript: env %q is not allowed", name)
	}
	return os.Getenv(name), nil
}

func (b *builtins) expandenv(s string) (string, error) {
	var denied string
	out := os.Expand(s, func(name string) string {
		if !b.host.EnvAllowed(name) {
			denied = name
			return ""
		}
		return os.Getenv(name)
	})
	if denied != "" {
		return "", fmt.Errorf("javascript: env %q is not allowed", denied)
	}
	return out, nil
}

func (b *builtins) getHostByName(name string) (string, error) {
	if !b.host.FetchAllowed(name) {
		return "", fmt.Errorf("javascript: host %q is not allowed", name)
	}
	addrs, err := net.LookupHost(name)
	if err != nil {
		return "", err
	}
	if len(addrs) == 0 {
		return "", fmt.Errorf("javascript: no address for %q", name)
	}
	return addrs[0], nil
}

func (b *builtins) readFile(name string) (string, error) {
	req := b.host.Request()
	if req == nil {
		return "", errors.New("javascript: readFile requires a request")
	}
	data, err := b.read(name)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (b *builtins) fileExists(name string) (bool, error) {
	req := b.host.Request()
	if req == nil {
		return false, errors.New("javascript: fileExists requires a request")
	}
	_, err := b.stat(name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (b *builtins) listFiles(name string) ([]string, error) {
	req := b.host.Request()
	if req == nil {
		return nil, errors.New("javascript: listFiles requires a request")
	}
	root := b.host.Root()
	if root == nil {
		return nil, errors.New("javascript: no sandbox root")
	}
	dir, err := root.Open(cleanSandboxName(name))
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names, nil
}

func (b *builtins) stripHTML(s string) string {
	return stripHTMLTags(s)
}

func (b *builtins) markdown(input any) (string, error) {
	var buf bytes.Buffer
	if err := goldmark.Convert([]byte(fmt.Sprint(input)), &buf); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func (b *builtins) httpError(status int) error {
	return &HTTPError{Status: status}
}

func (b *builtins) httpStatus(code int) string {
	return http.StatusText(code)
}

func (b *builtins) httpInclude(rawURL string) (string, error) {
	req := b.host.Request()
	if req == nil {
		return "", errors.New("javascript: httpInclude requires a request")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	if !u.IsAbs() {
		u.Scheme = "http"
		if req.TLS != nil {
			u.Scheme = "https"
		}
		u.Host = req.Host
	}
	if !b.host.FetchAllowed(u.Hostname()) {
		return "", fmt.Errorf("javascript: fetch to %q is not allowed", u.Hostname())
	}
	httpReq, err := http.NewRequestWithContext(req.Context(), http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func (b *builtins) include(name string) (string, error) {
	req := b.host.Request()
	if req == nil {
		return "", errors.New("javascript: include requires a request")
	}
	data, err := b.read(name)
	if err != nil {
		return "", err
	}
	tmpl, err := template.New(name).Funcs(b.registry.FuncMap()).Parse(string(data))
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, req); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func (b *builtins) read(name string) ([]byte, error) {
	root := b.host.Root()
	if root == nil {
		return nil, errors.New("javascript: no sandbox root")
	}
	f, err := root.Open(cleanSandboxName(name))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

func (b *builtins) stat(name string) (os.FileInfo, error) {
	root := b.host.Root()
	if root == nil {
		return nil, errors.New("javascript: no sandbox root")
	}
	f, err := root.Open(cleanSandboxName(name))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Stat()
}

// HTTPError is returned by httpError so the handler can translate it to a
// status code.
type HTTPError struct {
	Status int
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("javascript: http error %d", e.Status)
}

var (
	htmlTagPattern = regexp.MustCompile(`<[^>]*>`)
)

func stripHTMLTags(s string) string {
	return htmlTagPattern.ReplaceAllString(s, "")
}

func cleanSandboxName(name string) string {
	cleaned := path.Clean("/" + strings.TrimPrefix(name, "/"))
	return strings.TrimPrefix(cleaned, "/")
}

// FuncMap returns the registry as a Go template function map. It is used by
// include so the template engine can call the same functions.
func (r *Registry) FuncMap() map[string]any {
	m := make(map[string]any, len(r.funcs))
	for name, fn := range r.funcs {
		m[name] = fn.Interface()
	}
	return m
}
