package config

import (
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

func parse(t *testing.T, input string) (Options, error) {
	t.Helper()
	d := caddyfile.NewTestDispenser(input)
	var o Options
	o.ApplyDefaults()
	err := FromDispenser(d, &o)
	return o, err
}

func TestParseFull(t *testing.T) {
	o, err := parse(t, `javascript /srv/app/main.js {
		site main
		root /srv/app
		timeout 2s
		memory 67108864
		queue_size 8
		env_allow HOME E2E_SECRET
		fetch_allow 127.0.0.1 example.test
		watch false
		fallthrough
	}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if o.ScriptPath != "/srv/app/main.js" {
		t.Errorf("ScriptPath = %q", o.ScriptPath)
	}
	if o.Site != "main" {
		t.Errorf("Site = %q", o.Site)
	}
	if o.RootDir != "/srv/app" {
		t.Errorf("RootDir = %q", o.RootDir)
	}
	if time.Duration(o.Timeout) != 2*time.Second {
		t.Errorf("Timeout = %v", time.Duration(o.Timeout))
	}
	if o.Memory != 67108864 {
		t.Errorf("Memory = %d", o.Memory)
	}
	if o.QueueSize != 8 {
		t.Errorf("QueueSize = %d", o.QueueSize)
	}
	if strings.Join(o.EnvAllow, ",") != "HOME,E2E_SECRET" {
		t.Errorf("EnvAllow = %v", o.EnvAllow)
	}
	if strings.Join(o.FetchAllow, ",") != "127.0.0.1,example.test" {
		t.Errorf("FetchAllow = %v", o.FetchAllow)
	}
	if o.Watching() {
		t.Error("Watching() = true, want false")
	}
	if !o.Fallthrough {
		t.Error("Fallthrough = false, want true")
	}
}

func TestParseDefaults(t *testing.T) {
	o, err := parse(t, `javascript /srv/app/main.js`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if o.QueueSize != DefaultQueueSize {
		t.Errorf("QueueSize = %d", o.QueueSize)
	}
	if o.Memory != DefaultMemory {
		t.Errorf("Memory = %d", o.Memory)
	}
	if time.Duration(o.Timeout) != DefaultTimeout {
		t.Errorf("Timeout = %v", time.Duration(o.Timeout))
	}
	if !o.Watching() {
		t.Error("Watching() = false, want default true")
	}
	if err := o.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"missing path", `javascript`, "missing script path"},
		{"too many args", `javascript /a.js /b.js`, "too many arguments"},
		{"unknown option", `javascript /a.js {
			use app
		}`, "unrecognized option"},
		{"site missing value", `javascript /a.js {
			site
		}`, "javascript site: missing value"},
		{"root missing value", `javascript /a.js {
			root
		}`, "javascript root: missing path"},
		{"bad timeout", `javascript /a.js {
			timeout nope
		}`, "invalid duration"},
		{"bad memory", `javascript /a.js {
			memory nope
		}`, "invalid syntax"},
		{"bad queue", `javascript /a.js {
			queue_size nope
		}`, "invalid syntax"},
		{"bad watch", `javascript /a.js {
			watch nope
		}`, "invalid syntax"},
		{"empty env", `javascript /a.js {
			env_allow
		}`, "missing value"},
		{"empty fetch", `javascript /a.js {
			fetch_allow
		}`, "missing value"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parse(t, tt.input)
			if err == nil {
				t.Fatal("parse succeeded, want error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %q, want %q", err, tt.want)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	o := Options{ScriptPath: "/a.js", QueueSize: -1}
	o.ApplyDefaults()
	if err := o.Validate(); err == nil || !strings.Contains(err.Error(), "queue_size") {
		t.Fatalf("Validate = %v, want queue_size error", err)
	}

	o = Options{ScriptPath: "/a.js", Memory: -1}
	o.ApplyDefaults()
	if err := o.Validate(); err == nil || !strings.Contains(err.Error(), "memory") {
		t.Fatalf("Validate = %v, want memory error", err)
	}

	o = Options{ScriptPath: "/a.js", Timeout: -1}
	o.ApplyDefaults()
	if err := o.Validate(); err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("Validate = %v, want timeout error", err)
	}
}
