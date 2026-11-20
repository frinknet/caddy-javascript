package handler

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// autoReloadFloor keeps two VMs watching the same tree from loading the same
// configuration twice. Each site can hold several handlers, all watching the
// modules they share.
const autoReloadFloor = time.Second

var autoReloadGate struct {
	sync.Mutex
	last    time.Time
	pending bool
}

// autoReload fetches the active configuration and loads it again. Caddy
// provisions the new generation before retiring the old one, so a module that
// fails to evaluate leaves the previous generation serving.
//
// Several handlers can watch the same files. Calls inside the floor window are
// coalesced: the first one waits out the window and loads the newest state, so
// a quick second edit is not dropped.
func autoReload() error {
	autoReloadGate.Lock()
	wait := autoReloadFloor - time.Since(autoReloadGate.last)
	if wait <= 0 {
		autoReloadGate.last = time.Now()
		autoReloadGate.Unlock()
		return reloadActiveConfig()
	}
	if autoReloadGate.pending {
		autoReloadGate.Unlock()
		return nil
	}
	autoReloadGate.pending = true
	autoReloadGate.Unlock()

	time.Sleep(wait)

	autoReloadGate.Lock()
	autoReloadGate.pending = false
	autoReloadGate.last = time.Now()
	autoReloadGate.Unlock()
	return reloadActiveConfig()
}

func reloadActiveConfig() error {
	applyMu.Lock()
	defer applyMu.Unlock()

	doc, err := readAdminConfigRaw(5 * time.Second)
	if err != nil {
		return err
	}
	if len(doc) == 0 {
		return errors.New("javascript: reload: active configuration is empty")
	}
	return postAdminConfigForce(adminBase(), doc, true)
}

func readAdminConfigRaw(timeout time.Duration) ([]byte, error) {
	resp, err := adminClient(timeout).Get(adminBase() + "/config/")
	if err != nil {
		return nil, fmt.Errorf("javascript: read config: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("javascript: read config: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAdminConfig))
	if err != nil {
		return nil, fmt.Errorf("javascript: read config: %w", err)
	}
	return body, nil
}
