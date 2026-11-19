package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	defaultAdminAddr = "localhost:2019"
	adminTimeout     = 15 * time.Second
	maxAdminConfig   = 32 << 20
)

// applyMu serializes config loads. The worker is the only writer in this
// design; the lock still protects against two workers being provisioned at
// once during a reload.
var applyMu sync.Mutex

// applyCaddyConfig loads a whole Caddy configuration returned by config.apply.
func applyCaddyConfig(doc []byte) error {
	applyMu.Lock()
	defer applyMu.Unlock()

	if err := validateCaddyConfig(doc); err != nil {
		return err
	}
	return postAdminConfig(adminBase(), doc)
}

// validateCaddyConfig refuses an edit that would remove the global worker.
func validateCaddyConfig(doc []byte) error {
	var cfg map[string]any
	if err := json.Unmarshal(doc, &cfg); err != nil {
		return fmt.Errorf("javascript: config.apply: %w", err)
	}
	apps, _ := cfg["apps"].(map[string]any)
	if apps == nil {
		return errors.New("javascript: config.apply: apps would be removed")
	}
	if _, ok := apps["javascript"]; !ok {
		return errors.New("javascript: config.apply: apps.javascript would be removed")
	}
	return nil
}

func adminBase() string {
	addr := os.Getenv("CADDY_ADMIN")
	if addr == "" {
		addr = defaultAdminAddr
	}
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	return strings.TrimSuffix(addr, "/")
}

func adminClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout}
}

// readAdminConfig fetches the active configuration. It is used to seed the
// worker's config object once Caddy's admin endpoint is listening.
func readAdminConfig(timeout time.Duration) (map[string]any, error) {
	resp, err := adminClient(timeout).Get(adminBase() + "/config/")
	if err != nil {
		return nil, fmt.Errorf("javascript: read config: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("javascript: read config: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAdminConfig))
	if err != nil {
		return nil, fmt.Errorf("javascript: read config: %w", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(body, &cfg); err != nil {
		return nil, fmt.Errorf("javascript: read config: %w", err)
	}
	return cfg, nil
}

func postAdminConfig(base string, body []byte) error {
	req, err := http.NewRequest(http.MethodPost, base+"/load", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := adminClient(adminTimeout).Do(req)
	if err != nil {
		return fmt.Errorf("javascript: config.apply: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("javascript: config.apply: status %d: %s", resp.StatusCode, strings.TrimSpace(string(message)))
	}
	return nil
}
