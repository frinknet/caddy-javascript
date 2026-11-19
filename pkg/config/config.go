package config

import (
	"fmt"
	"strconv"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

const (
	DefaultTimeout   = 3 * time.Second
	DefaultMemory    = int64(64 << 20)
	DefaultQueueSize = 256
)

type Options struct {
	ScriptPath string         `json:"script_path,omitempty"`
	Site       string         `json:"site,omitempty"`
	RootDir    string         `json:"root,omitempty"`
	Timeout    caddy.Duration `json:"timeout,omitempty"`
	Memory     int64          `json:"memory,omitempty"`
	QueueSize  int            `json:"queue_size,omitempty"`
	EnvAllow   []string       `json:"env_allow,omitempty"`
	FetchAllow []string       `json:"fetch_allow,omitempty"`
	Watch      *bool          `json:"watch,omitempty"`
}

func (o *Options) ApplyDefaults() {
	if o.Timeout == 0 {
		o.Timeout = caddy.Duration(DefaultTimeout)
	}
	if o.Memory == 0 {
		o.Memory = DefaultMemory
	}
	if o.QueueSize == 0 {
		o.QueueSize = DefaultQueueSize
	}
}

func (o Options) Validate() error {
	if o.ScriptPath == "" {
		return fmt.Errorf("javascript: script path is required")
	}
	if o.Timeout < 0 {
		return fmt.Errorf("javascript: timeout must not be negative")
	}
	if o.Memory < 0 {
		return fmt.Errorf("javascript: memory must not be negative")
	}
	if o.QueueSize < 1 {
		return fmt.Errorf("javascript: queue_size must be at least 1")
	}
	return nil
}

func (o Options) Watching() bool {
	return o.Watch == nil || *o.Watch
}

func FromDispenser(d *caddyfile.Dispenser, o *Options) error {
	if !d.Next() {
		return d.ArgErr()
	}

	args := d.RemainingArgs()
	switch len(args) {
	case 0:
		return fmt.Errorf("javascript: missing script path")
	case 1:
		o.ScriptPath = args[0]
	default:
		return fmt.Errorf("javascript: too many arguments, expected only a script path")
	}

	for d.NextBlock(0) {
		switch d.Val() {
		case "site":
			if !d.NextArg() {
				return fmt.Errorf("javascript site: missing value")
			}
			o.Site = d.Val()
		case "root":
			if !d.NextArg() {
				return fmt.Errorf("javascript root: missing path")
			}
			o.RootDir = d.Val()
		case "timeout":
			if !d.NextArg() {
				return fmt.Errorf("javascript timeout: missing duration")
			}
			dur, err := time.ParseDuration(d.Val())
			if err != nil {
				return fmt.Errorf("javascript timeout: %w", err)
			}
			o.Timeout = caddy.Duration(dur)
		case "memory":
			if !d.NextArg() {
				return fmt.Errorf("javascript memory: missing value")
			}
			n, err := strconv.ParseInt(d.Val(), 10, 64)
			if err != nil {
				return fmt.Errorf("javascript memory: %w", err)
			}
			o.Memory = n
		case "queue_size":
			if !d.NextArg() {
				return fmt.Errorf("javascript queue_size: missing value")
			}
			n, err := strconv.Atoi(d.Val())
			if err != nil {
				return fmt.Errorf("javascript queue_size: %w", err)
			}
			o.QueueSize = n
		case "env_allow":
			vals := d.RemainingArgs()
			if len(vals) == 0 {
				return fmt.Errorf("javascript env_allow: missing value")
			}
			o.EnvAllow = append(o.EnvAllow, vals...)
		case "fetch_allow":
			vals := d.RemainingArgs()
			if len(vals) == 0 {
				return fmt.Errorf("javascript fetch_allow: missing value")
			}
			o.FetchAllow = append(o.FetchAllow, vals...)
		case "watch":
			if d.NextArg() {
				b, err := strconv.ParseBool(d.Val())
				if err != nil {
					return fmt.Errorf("javascript watch: %w", err)
				}
				o.Watch = &b
			} else {
				on := true
				o.Watch = &on
			}
		default:
			return fmt.Errorf("javascript: unrecognized option %q", d.Val())
		}
	}

	return nil
}
