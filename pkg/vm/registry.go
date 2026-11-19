package vm

import (
	"context"
	"sync"
)

// Registry owns the VMs of one configuration generation. Sites join the VM
// already registered under their key, so provisioning order does not matter.
// A new generation gets a new registry, and therefore a new VM.
type Registry struct {
	mu    sync.Mutex
	sites map[string]*siteEntry
}

type siteEntry struct {
	vm   *VM
	refs int
}

func NewRegistry() *Registry {
	return &Registry{sites: make(map[string]*siteEntry)}
}

// Join returns the VM registered for key, evaluating cfg.ScriptPath in it.
// The first caller creates the VM with cfg; later callers load their module
// into the existing runtime and share its realm and ESM cache.
func (r *Registry) Join(ctx context.Context, key string, cfg Config) (*VM, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if entry := r.sites[key]; entry != nil {
		if err := entry.vm.Load(ctx, cfg.ScriptPath); err != nil {
			return nil, err
		}
		entry.refs++
		return entry.vm, nil
	}

	v, err := New(cfg)
	if err != nil {
		return nil, err
	}
	r.sites[key] = &siteEntry{vm: v, refs: 1}
	return v, nil
}

// Release drops one reference to key and closes its VM when none remain.
func (r *Registry) Release(key string) error {
	r.mu.Lock()
	entry := r.sites[key]
	if entry == nil {
		r.mu.Unlock()
		return nil
	}
	entry.refs--
	if entry.refs > 0 {
		r.mu.Unlock()
		return nil
	}
	delete(r.sites, key)
	r.mu.Unlock()

	return entry.vm.Close()
}
