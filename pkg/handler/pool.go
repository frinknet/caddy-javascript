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
// running VM instead of paying for a new realm and module evaluation, unless a
// module the VM loaded was edited; then the old VM stays with its generation
// and a fresh VM takes the stable key, so an in-flight request is never moved
// to a runtime that no longer matches.
type vmPool struct {
	sync.Mutex
	entries map[string]*poolEntry
	current map[string]string
	seq     uint64
}

type poolEntry struct {
	vm   *vm.VM
	refs int
	base string
}

var (
	sitePool = newVMPool()
	appPool  = newVMPool()
)

func newVMPool() vmPool {
	return vmPool{
		entries: make(map[string]*poolEntry),
		current: make(map[string]string),
	}
}

// acquire joins the pool under base or creates its VM. It returns the exact key
// the caller must release, which differs from base once an edit replaced the
// VM. reused reports whether an existing VM served the join.
func (p *vmPool) acquire(ctx context.Context, base string, cfg vm.Config) (*vm.VM, string, bool, error) {
	p.Lock()
	defer p.Unlock()

	key := base
	if current := p.current[base]; current != "" {
		key = current
	}
	entry := p.entries[key]
	if entry != nil && entry.vm.TreeChanged() {
		key = p.nextKey(base)
		p.current[base] = key
		entry = nil
	}

	if entry != nil {
		entry.refs++
		if err := entry.vm.Join(ctx, cfg); err != nil {
			entry.refs--
			if entry.refs == 0 {
				delete(p.entries, key)
				if p.current[base] == key {
					delete(p.current, base)
				}
				go entry.vm.Close()
			}
			return nil, key, false, err
		}
		return entry.vm, key, true, nil
	}

	v, err := vm.New(cfg)
	if err != nil {
		if p.current[base] == key {
			delete(p.current, base)
		}
		return nil, key, false, err
	}
	p.entries[key] = &poolEntry{vm: v, refs: 1, base: base}
	p.current[base] = key
	return v, key, false, nil
}

func (p *vmPool) nextKey(base string) string {
	p.seq++
	return fmt.Sprintf("%s#tree:%d", base, p.seq)
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
	if p.current[entry.base] == key {
		delete(p.current, entry.base)
	}
	p.Unlock()

	return entry.vm.Close()
}

// acquireSite joins the pool for a site VM. A host or explicit site key is
// stable; a hostless site uses a matcher fingerprint so a mount that shifts
// the route index does not replace the VM.
func acquireSite(ctx context.Context, site siteRef, cfg vm.Config) (*vm.VM, string, error) {
	v, key, _, err := sitePool.acquire(ctx, sitePoolKey(site, cfg), cfg)
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
	return appPool.acquire(ctx, appPoolKey(cfg), cfg)
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
