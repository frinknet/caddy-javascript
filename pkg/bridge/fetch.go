package bridge

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/grafana/sobek"
)

const (
	fetchTimeout  = 10 * time.Second
	maxFetchBytes = 1 << 20
	maxRedirects  = 5
)

func (p *Program) registerFetch() {
	client := &http.Client{
		Timeout: fetchTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			return p.checkFetchTarget(req.URL.Hostname())
		},
	}

	p.rt.Set("fetch", func(call sobek.FunctionCall) sobek.Value {
		rawURL := call.Argument(0).String()
		cb, ok := sobek.AssertFunction(call.Argument(1))
		if !ok {
			panic(p.rt.NewTypeError("fetch requires a callback"))
		}

		ex := p.trackAsync()

		u, err := url.Parse(rawURL)
		if err != nil {
			p.deliverFetch(ex, cb, fmt.Errorf("invalid url: %w", err), 0, "")
			return sobek.Undefined()
		}
		if err := p.checkFetchTarget(u.Hostname()); err != nil {
			p.deliverFetch(ex, cb, err, 0, "")
			return sobek.Undefined()
		}

		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
			defer cancel()

			req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
			if err != nil {
				p.deliverFetch(ex, cb, err, 0, "")
				return
			}

			resp, err := client.Do(req)
			if err != nil {
				p.deliverFetch(ex, cb, err, 0, "")
				return
			}
			defer resp.Body.Close()

			body, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchBytes+1))
			if err != nil {
				p.deliverFetch(ex, cb, err, 0, "")
				return
			}
			if int64(len(body)) > maxFetchBytes {
				p.deliverFetch(ex, cb, fmt.Errorf("response exceeds %d bytes", maxFetchBytes), 0, "")
				return
			}

			p.deliverFetch(ex, cb, nil, resp.StatusCode, string(body))
		}()

		return sobek.Undefined()
	})
}

func (p *Program) checkFetchTarget(host string) error {
	host = strings.TrimSuffix(host, ".")
	if host == "" {
		return fmt.Errorf("fetch blocked: missing host")
	}

	allowAll := false
	for _, allowed := range p.cfg.FetchAllow {
		if allowed == "*" {
			allowAll = true
			continue
		}
		if strings.EqualFold(allowed, host) {
			return nil
		}
	}

	if !allowAll {
		return fmt.Errorf("fetch blocked: %s is not in fetch_allow", host)
	}

	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("fetch blocked: cannot resolve %s: %w", host, err)
	}
	for _, ip := range ips {
		if !isPublicIP(ip) {
			return fmt.Errorf("fetch blocked: %s resolves to a private address", host)
		}
	}
	return nil
}

func isPublicIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsMulticast() {
		return false
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false
	}
	return true
}

func (p *Program) deliverFetch(ex *Exchange, cb sobek.Callable, err error, status int, body string) {
	p.submitAsync(ex, func() {
		if err != nil {
			_, _ = cb(sobek.Undefined(), p.rt.ToValue(err.Error()), sobek.Null())
			return
		}
		res := p.rt.NewObject()
		_ = res.Set("status", status)
		_ = res.Set("text", body)
		_, _ = cb(sobek.Undefined(), sobek.Null(), res)
	})
}
