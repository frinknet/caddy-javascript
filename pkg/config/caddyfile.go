package config

import (
	"fmt"
	"strconv"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
)

func Parse(h httpcaddyfile.Helper) (Options, error) {
	var opts Options
	opts.ApplyDefaults()

	for h.Next() {
		args := h.RemainingArgs()
		switch len(args) {
		case 0:
			return opts, fmt.Errorf("javascript: missing script path")
		case 1:
			opts.ScriptPath = args[0]
		default:
			return opts, fmt.Errorf("javascript: too many arguments, expected only a script path")
		}

		for h.NextBlock(0) {
			switch h.Val() {
			case "root":
				if !h.NextArg() {
					return opts, fmt.Errorf("javascript root: missing path")
				}
				opts.RootDir = h.Val()
			case "timeout":
				if !h.NextArg() {
					return opts, fmt.Errorf("javascript timeout: missing duration")
				}
				d, err := time.ParseDuration(h.Val())
				if err != nil {
					return opts, fmt.Errorf("javascript timeout: %w", err)
				}
				opts.Timeout = caddy.Duration(d)
			case "queue_size":
				if !h.NextArg() {
					return opts, fmt.Errorf("javascript queue_size: missing value")
				}
				n, err := strconv.Atoi(h.Val())
				if err != nil {
					return opts, fmt.Errorf("javascript queue_size: %w", err)
				}
				opts.QueueSize = n
			case "max_body":
				if !h.NextArg() {
					return opts, fmt.Errorf("javascript max_body: missing value")
				}
				n, err := strconv.ParseInt(h.Val(), 10, 64)
				if err != nil {
					return opts, fmt.Errorf("javascript max_body: %w", err)
				}
				opts.MaxBody = n
			case "watch":
				if h.NextArg() {
					b, err := strconv.ParseBool(h.Val())
					if err != nil {
						return opts, fmt.Errorf("javascript watch: %w", err)
					}
					opts.Watch = &b
				} else {
					on := true
					opts.Watch = &on
				}
			case "fallthrough":
				opts.Fallthrough = true
			case "env_allow":
				opts.EnvAllow = append(opts.EnvAllow, h.RemainingArgs()...)
			case "fetch_allow":
				opts.FetchAllow = append(opts.FetchAllow, h.RemainingArgs()...)
			default:
				return opts, fmt.Errorf("javascript: unrecognized option %q", h.Val())
			}
		}
	}

	return opts, nil
}
