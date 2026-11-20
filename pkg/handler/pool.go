package handler

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/frinknet/caddy-javascript/pkg/vm"
)

// Pools keep VMs alive across configuration generations. A reload adopts a
// running VM instead of paying for a new realm and module evaluation.
type vmPool struct {
	sync.Mutex
	entries map[string]*poolEntry
}

type poolEntry struct {
	vm   *vm.VM
	refs int
}

var (
	sitePool = vmPool{entries: make(map[string]*poolEntry)}
	appPool  = vmPool{entries: make(map[string]*poolEntry)}
)

// acquire joins the pool for key or creates its VM and reports whether it
// reused an existing one.
func (p *vmPool) acquire(ctx context.Context, key string, cfg vm.Config) (*vm.VM, bool, error) {
	p.Lock()
	defer p.Unlock()

	if entry := p.entries[key]; entry != nil {
		entry.refs++
		if err := entry.vm.Join(ctx, cfg); err != nil {
			entry.refs--
			if entry.refs == 0 {
				delete(p.entries, key)
				go entry.vm.Close()
			}
			return nil, false, err
		}
		return entry.vm, true, nil
	}

	v, err := vm.New(cfg)
	if err != nil {
		return nil, false, err
	}
	p.entries[key] = &poolEntry{vm: v, refs: 1}
	return v, false, nil
}

func (p *vmPool) release(key string) error {
	if key == "" {
		return nil
	}
	p.Lock()
	entry := p.entries[key]
	if entry == nil {
		p.Unlock()
		return nil
	}
	entry.refs--
	if entry.refs > 0 {
		p.Unlock()
		return nil
	}
	delete(p.entries, key)
	p.Unlock()

	return entry.vm.Close()
}

// acquireSite joins the pool for a site VM. A host or explicit site key is
// stable; a hostless site uses a matcher fingerprint so a mount that shifts
// the route index does not replace the VM.
func acquireSite(ctx context.Context, site siteRef, cfg vm.Config) (*vm.VM, string, error) {
	key := sitePoolKey(site, cfg)
	v, _, err := sitePool.acquire(ctx, key, cfg)
	return v, key, err
}

func releaseSite(key string) error {
	return sitePool.release(key)
}

func sitePoolKey(site siteRef, cfg vm.Config) string {
	listen := append([]string(nil), site.srv.Listen...)
	sort.Strings(listen)
	allow := append([]string(nil), cfg.FetchAllow...)
	sort.Strings(allow)
	env := append([]string(nil), cfg.EnvAllow...)
	sort.Strings(env)
	return fmt.Sprintf("%s|%s|%s|%d|%d|%s|%s",
		strings.Join(listen, ","), site.identity, cfg.RootDir, cfg.Memory, cfg.QueueSize,
		strings.Join(allow, ","), strings.Join(env, ","))
}

// acquireApp joins the pool for the global worker VM.
func acquireApp(ctx context.Context, cfg vm.Config) (*vm.VM, string, bool, error) {
	key := appPoolKey(cfg)
	v, reused, err := appPool.acquire(ctx, key, cfg)
	return v, key, reused, err
}

func releaseApp(key string) error {
	return appPool.release(key)
}

func appPoolKey(cfg vm.Config) string {
	allow := append([]string(nil), cfg.FetchAllow...)
	sort.Strings(allow)
	return fmt.Sprintf("app|%s|%s|%d|%d|%s",
		cfg.ScriptPath, cfg.RootDir, cfg.Memory, cfg.QueueSize, strings.Join(allow, ","))
}
