package config

import (
	"fmt"
	"time"

	"github.com/caddyserver/caddy/v2"
)

const (
	DefaultTimeout   = 3 * time.Second
	DefaultQueueSize = 5000
	DefaultMaxBody   = 1 << 20
)

type Options struct {
	ScriptPath  string         `json:"script_path,omitempty"`
	RootDir     string         `json:"root,omitempty"`
	Timeout     caddy.Duration `json:"timeout,omitempty"`
	QueueSize   int            `json:"queue_size,omitempty"`
	Watch       *bool          `json:"watch,omitempty"`
	EnvAllow    []string       `json:"env_allow,omitempty"`
	FetchAllow  []string       `json:"fetch_allow,omitempty"`
	MaxBody     int64          `json:"max_body,omitempty"`
	Fallthrough bool           `json:"fallthrough,omitempty"`
}

func (o *Options) ApplyDefaults() {
	if o.Timeout == 0 {
		o.Timeout = caddy.Duration(DefaultTimeout)
	}
	if o.QueueSize == 0 {
		o.QueueSize = DefaultQueueSize
	}
	if o.MaxBody == 0 {
		o.MaxBody = DefaultMaxBody
	}
}

func (o Options) Validate() error {
	if o.ScriptPath == "" {
		return fmt.Errorf("javascript: script path is required")
	}
	if o.Timeout < 0 {
		return fmt.Errorf("javascript: timeout must not be negative")
	}
	if o.QueueSize < 1 {
		return fmt.Errorf("javascript: queue_size must be at least 1")
	}
	if o.MaxBody < 0 {
		return fmt.Errorf("javascript: max_body must not be negative")
	}
	return nil
}

func (o Options) Watching() bool {
	return o.Watch == nil || *o.Watch
}
