package template

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"
	"unicode"

	"github.com/BurntSushi/toml"
	"github.com/Masterminds/sprig/v3"
	"github.com/dustin/go-humanize"
	"github.com/yuin/goldmark"
	"gopkg.in/yaml.v3"
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
		{"fileStat", b.fileStat, true},
		{"listFiles", b.listFiles, true},
		{"stripHTML", b.stripHTML, false},
		{"markdown", b.markdown, false},
		{"splitFrontMatter", b.splitFrontMatter, false},
		{"humanize", b.humanize, false},
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

func (b *builtins) fileStat(name string) (FileInfo, error) {
	req := b.host.Request()
	if req == nil {
		return FileInfo{}, errors.New("javascript: fileStat requires a request")
	}
	info, err := b.stat(name)
	if err != nil {
		return FileInfo{}, err
	}
	return FileInfo{
		Name:    info.Name(),
		Size:    info.Size(),
		Mode:    info.Mode().String(),
		ModTime: info.ModTime().Format(time.RFC3339),
		IsDir:   info.IsDir(),
	}, nil
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
	if err := goldmark.Convert([]byte(stringValue(input)), &buf); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func (b *builtins) splitFrontMatter(input any) (FrontMatter, error) {
	meta, body, err := extractFrontMatter(stringValue(input))
	if err != nil {
		return FrontMatter{}, err
	}
	return FrontMatter{Meta: meta, Body: body}, nil
}

func (b *builtins) humanize(formatType, data string) (string, error) {
	parts := strings.SplitN(formatType, ":", 2)
	switch parts[0] {
	case "size":
		n, err := strconv.ParseUint(data, 10, 64)
		if err != nil {
			return "", fmt.Errorf("humanize: size cannot be parsed: %w", err)
		}
		return humanize.Bytes(n), nil
	case "time":
		layout := time.RFC1123Z
		if len(parts) > 1 {
			layout = parts[1]
		}
		t, err := time.Parse(layout, data)
		if err != nil {
			return "", fmt.Errorf("humanize: time cannot be parsed: %w", err)
		}
		return humanize.Time(t), nil
	}
	return "", fmt.Errorf("humanize: unknown format %q", parts[0])
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

// FileInfo is the plain value fileStat returns to JavaScript. The mod time is
// formatted as RFC 3339 because a time.Time does not cross the boundary.
type FileInfo struct {
	Name    string
	Size    int64
	Mode    string
	ModTime string
	IsDir   bool
}

// FrontMatter is the split result of splitFrontMatter: the parsed metadata and
// the body after the closing fence.
type FrontMatter struct {
	Meta map[string]any
	Body string
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

func stringValue(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []byte:
		return string(x)
	default:
		return fmt.Sprint(x)
	}
}

type frontMatterType struct {
	fenceOpen  string
	fenceClose []string
	parse      func([]byte) (map[string]any, error)
}

var supportedFrontMatterTypes = []frontMatterType{
	{
		fenceOpen:  "---",
		fenceClose: []string{"---", "..."},
		parse:      yamlFrontMatter,
	},
	{
		fenceOpen:  "+++",
		fenceClose: []string{"+++"},
		parse:      tomlFrontMatter,
	},
	{
		fenceOpen:  "{",
		fenceClose: []string{"}"},
		parse:      jsonFrontMatter,
	},
}

func extractFrontMatter(input string) (map[string]any, string, error) {
	var firstLineStart, firstLineEnd int
	lineEmpty := true
	for i, b := range input {
		if b == '\n' {
			firstLineStart = firstLineEnd
			if firstLineStart > 0 {
				firstLineStart++
			}
			firstLineEnd = i
			if !lineEmpty {
				break
			}
			continue
		}
		lineEmpty = lineEmpty && unicode.IsSpace(b)
	}
	firstLine := strings.TrimSpace(input[firstLineStart:firstLineEnd])

	var closingFence []string
	var parse func([]byte) (map[string]any, error)
	for _, fmType := range supportedFrontMatterTypes {
		if firstLine == fmType.fenceOpen {
			closingFence = fmType.fenceClose
			parse = fmType.parse
			break
		}
	}
	if parse == nil {
		return nil, input, nil
	}

	var fmEndFence string
	fmEndFenceStart := -1
	for _, fence := range closingFence {
		index := strings.Index(input[firstLineEnd:], "\n"+fence)
		if index >= 0 && (fmEndFenceStart < 0 || index < fmEndFenceStart) {
			fmEndFenceStart = index
			fmEndFence = fence
		}
	}
	if fmEndFenceStart < 0 {
		return nil, "", errors.New("unterminated front matter")
	}
	fmEndFenceStart += firstLineEnd + 1

	frontMatter := input[firstLineEnd:fmEndFenceStart]
	meta, err := parse([]byte(frontMatter))
	if err != nil {
		return nil, "", err
	}
	body := input[fmEndFenceStart+len(fmEndFence):]
	return meta, body, nil
}

func yamlFrontMatter(input []byte) (map[string]any, error) {
	m := make(map[string]any)
	err := yaml.Unmarshal(input, &m)
	return m, err
}

func tomlFrontMatter(input []byte) (map[string]any, error) {
	m := make(map[string]any)
	err := toml.Unmarshal(input, &m)
	return m, err
}

func jsonFrontMatter(input []byte) (map[string]any, error) {
	input = append([]byte{'{'}, input...)
	input = append(input, '}')
	m := make(map[string]any)
	err := json.Unmarshal(input, &m)
	return m, err
}

func cleanSandboxName(name string) string {
	cleaned := path.Clean("/" + strings.TrimPrefix(name, "/"))
	cleaned = strings.TrimPrefix(cleaned, "/")
	if cleaned == "" {
		return "."
	}
	return cleaned
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
