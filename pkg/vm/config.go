package vm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/caddyserver/caddy/v2/caddyconfig"
	_ "github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"go.uber.org/zap"
)

// CaddyConfig seeds the worker's `config` global. Value is the whole Caddy
// configuration; Apply posts an edited copy back through the admin endpoint.
type CaddyConfig struct {
	Value map[string]any
	Apply func(doc []byte) error
}

// configureConfig installs `config` or attaches the apply callback to an
// already running worker. It runs on the VM goroutine.
func (v *VM) configureConfig(doc *CaddyConfig) error {
	if doc == nil {
		return nil
	}
	if !v.hasConfig {
		obj := v.rt.NewObject()
		if err := obj.Set("apply", v.configApply); err != nil {
			return err
		}
		if err := obj.Set("caddyfile", v.configCaddyfile); err != nil {
			return err
		}
		if err := v.rt.Set("config", obj); err != nil {
			return err
		}
		stringify, err := v.rt.Eval("JSON.stringify")
		if err != nil {
			return err
		}
		deleteProp, err := v.rt.Eval("(function (object, key) { delete object[key] })")
		if err != nil {
			return err
		}
		v.config = obj
		v.stringify = stringify
		v.deleteProp = deleteProp
		v.hasConfig = true
	}
	if doc.Apply != nil {
		v.apply = doc.Apply
	}
	v.reloading = false
	if doc.Value == nil {
		return nil
	}

	for _, key := range v.config.Keys() {
		if key == "apply" || key == "caddyfile" {
			continue
		}
		if _, err := v.deleteProp.Call(v.config, key); err != nil {
			return err
		}
	}
	for key, value := range doc.Value {
		if key == "apply" {
			continue
		}
		if err := v.config.Set(key, value); err != nil {
			return err
		}
	}
	applied, err := canonicalValue(doc.Value)
	if err != nil {
		return err
	}
	v.applied = applied
	return nil
}

// configCaddyfile adapts a Caddyfile from the sandbox into a plain config
// object. It does not touch the running configuration; call config.apply()
// after merging the result.
func (v *VM) configCaddyfile(path string) (map[string]any, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	name, err := v.sandbox.name(abs)
	if err != nil {
		return nil, err
	}
	src, err := v.sandbox.read(name)
	if err != nil {
		return nil, err
	}
	adapter := caddyconfig.GetAdapter("caddyfile")
	if adapter == nil {
		return nil, errors.New("javascript: caddyfile adapter is not registered")
	}
	out, warnings, err := adapter.Adapt(src, map[string]any{"filename": abs})
	for _, warning := range warnings {
		if v.logger != nil {
			v.logger.Warn("javascript: caddyfile warning",
				zap.String("file", warning.File),
				zap.Int("line", warning.Line),
				zap.String("message", warning.Message),
			)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("javascript: adapt %s: %w", abs, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("javascript: adapt %s: %w", abs, err)
	}
	return doc, nil
}

// configJSON serializes the worker's config object canonically. The apply
// method is a function and JSON.stringify drops it.
func (v *VM) configJSON() ([]byte, error) {
	if !v.hasConfig {
		return nil, errors.New("javascript: config is not available")
	}
	encoded, err := v.stringify.Call(v.config)
	if err != nil {
		return nil, err
	}
	return canonicalJSON([]byte(encoded.String()))
}

// configApply is config.apply(). It validates and hands the whole document to
// the handler, which loads it off the VM goroutine.
func (v *VM) configApply() (bool, error) {
	if !v.hasConfig {
		return false, errors.New("javascript: config is not available")
	}
	if v.apply == nil {
		return false, errors.New("javascript: config.apply requires an admin endpoint")
	}
	if v.reloading {
		return false, nil
	}
	current, err := v.configJSON()
	if err != nil {
		return false, err
	}
	if bytes.Equal(current, v.applied) {
		return false, nil
	}
	if err := validateConfigDoc(current); err != nil {
		return false, err
	}
	v.applied = current
	v.reloading = true
	apply := v.apply
	doc := append([]byte(nil), current...)
	logger := v.logger
	go func() {
		if err := apply(doc); err == nil {
			return
		} else if logger != nil {
			logger.Error("javascript: config.apply failed", zap.Error(err))
		}
		v.loop.Post(func() { v.reloading = false })
	}()
	return true, nil
}

// configEdited reports whether a script changed the configuration since it was
// seeded.
func (v *VM) configEdited() (bool, error) {
	if !v.hasConfig || v.applied == nil {
		return false, nil
	}
	current, err := v.configJSON()
	if err != nil {
		return false, err
	}
	return !bytes.Equal(current, v.applied), nil
}

// SetConfig installs or refreshes the worker's config from another goroutine.
// It is how the app seeds a config object once Caddy's admin endpoint is up.
func (v *VM) SetConfig(ctx context.Context, doc *CaddyConfig) error {
	ch := make(chan error, 1)
	v.loop.Post(func() { ch <- v.configureConfig(doc) })
	select {
	case err := <-ch:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-v.done:
		if err := v.runError(); err != nil {
			return err
		}
		return ErrClosed
	}
}

// HasConfig reports whether the VM exposes a config object.
func (v *VM) HasConfig() bool {
	return v.hasConfig
}

// validateConfigDoc rejects a whole-config edit that would remove the worker
// itself.
func validateConfigDoc(data []byte) error {
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("javascript: config.apply: %w", err)
	}
	apps, _ := doc["apps"].(map[string]any)
	if apps == nil {
		return errors.New("javascript: config.apply: apps would be removed")
	}
	if _, ok := apps["javascript"]; !ok {
		return errors.New("javascript: config.apply: apps.javascript would be removed")
	}
	return nil
}

func canonicalValue(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return canonicalJSON(raw)
}

// canonicalJSON decodes and re-encodes JSON so key order and whitespace do not
// affect comparisons.
func canonicalJSON(data []byte) ([]byte, error) {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}
