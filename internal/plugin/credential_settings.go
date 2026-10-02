// Created by Darvin.
package plugin

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"cpa-key-billing-plus/internal/billing"
	"cpa-key-billing-plus/internal/tasksettings"
)

const credentialRequestHeader = "X-Cpa-Billing-Plus-Request"

type credentialControls struct {
	mu          sync.Mutex
	settings    tasksettings.Settings
	path        string
	active      map[string]string
	counts      map[string]int
	worker      *os.Process
	origin, key string // Management session only: never persist or return credentials.
}

func (c *credentialControls) configure(path string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.path == path {
		return nil
	}
	cfg := tasksettings.Default()
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		if err = json.Unmarshal(raw, &cfg); err != nil {
			return err
		}
	}
	if err = cfg.Validate(); err != nil {
		return err
	}
	c.stopWorker()
	c.path, c.settings = path, cfg
	if c.active == nil {
		c.active = map[string]string{}
		c.counts = map[string]int{}
	}
	return nil
}

func (c *credentialControls) stopWorker() {
	if c.worker != nil {
		_ = c.worker.Kill()
		_, _ = c.worker.Wait()
		c.worker = nil
	}
}

// The timer lives in a separate executable, keeping the embedded Go runtime idle between host calls.
func (c *credentialControls) startWorker(origin, key string) error {
	if !c.settings.Enabled {
		return nil
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return fmt.Errorf("task origin must be a loopback HTTP origin")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("task origin must be loopback")
	}
	if key == "" {
		return fmt.Errorf("management session is required")
	}
	if c.worker != nil && c.origin == origin && c.key == key {
		return nil
	}
	worker := filepath.Join("plugins", "cpa-key-billing-plus-worker")
	if runtime.GOOS == "windows" {
		worker += ".exe"
	}
	if _, err := os.Stat(worker); err != nil {
		return fmt.Errorf("install the task worker in plugins before enabling tasks")
	}
	input, output, err := os.Pipe()
	if err != nil {
		return err
	}
	defer input.Close()
	defer output.Close()
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer null.Close()
	c.stopWorker()
	proc, err := os.StartProcess(worker, []string{worker}, &os.ProcAttr{Env: os.Environ(), Files: []*os.File{input, null, null}})
	if err != nil {
		return err
	}
	if err := json.NewEncoder(output).Encode(map[string]any{"origin": origin, "key": key, "parent": os.Getpid()}); err != nil {
		_ = proc.Kill()
		_, _ = proc.Wait()
		return err
	}
	c.worker, c.origin, c.key = proc, origin, key
	return nil
}

func (a *App) credentialSettings(req ManagementRequest) ManagementResponse {
	c := &a.controls
	c.mu.Lock()
	defer c.mu.Unlock()
	if req.Method == http.MethodPut {
		cfg := tasksettings.Default()
		if err := json.Unmarshal(req.Body, &cfg); err != nil {
			return JSONError(400, "invalid", err.Error())
		}
		if err := cfg.Validate(); err != nil {
			return JSONError(400, "invalid", err.Error())
		}
		raw, _ := json.MarshalIndent(cfg, "", "  ")
		temp, err := os.CreateTemp(filepath.Dir(c.path), ".plus-settings-*")
		if err != nil {
			return JSONError(500, "save_failed", err.Error())
		}
		defer os.Remove(temp.Name())
		_, err = temp.Write(raw)
		if err == nil {
			err = temp.Sync()
		}
		closeErr := temp.Close()
		if err == nil {
			err = closeErr
		}
		if err == nil {
			err = os.Rename(temp.Name(), c.path)
		}
		if err != nil {
			return JSONError(500, "save_failed", err.Error())
		}
		c.settings = cfg
	}
	// GET without an origin is a read-only worker poll. An admin session can resume after host restart.
	if req.Query.Get("origin") != "" {
		key := strings.TrimPrefix(req.Headers.Get("Authorization"), "Bearer ")
		if err := c.startWorker(req.Query.Get("origin"), key); err != nil {
			return JSONResponse(200, map[string]any{"settings": c.settings, "running": false, "error": err.Error()})
		}
	}
	return JSONResponse(200, map[string]any{"settings": c.settings, "running": c.worker != nil && c.settings.Enabled})
}

func (a *App) credentialLimit() int {
	a.controls.mu.Lock()
	defer a.controls.mu.Unlock()
	return a.controls.settings.MaxConcurrency
}

func (a *App) releaseCredential(requestID string) {
	c := &a.controls
	c.mu.Lock()
	defer c.mu.Unlock()
	id, exists := c.active[requestID]
	if exists {
		delete(c.active, requestID)
	}
	if id != "" {
		c.counts[id]--
		if c.counts[id] <= 0 {
			delete(c.counts, id)
		}
	}
}

// Probe requests share capacity with regular requests and always resolve current host identifiers.
func (a *App) credentialTaskSlot(req ManagementRequest) ManagementResponse {
	var body struct {
		AuthIndex string `json:"auth_index"`
		RequestID string `json:"request_id"`
		Release   bool   `json:"release"`
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return JSONError(400, "invalid", err.Error())
	}
	if body.RequestID == "" || !strings.HasPrefix(body.RequestID, "probe:") || len(body.RequestID) > 128 {
		return JSONError(400, "invalid", "Invalid test request identity")
	}
	if body.Release {
		a.releaseCredential(body.RequestID)
		return JSONResponse(200, map[string]bool{"released": true})
	}
	files, err := a.listHostAuthFiles()
	if err != nil {
		return JSONError(502, "host_unavailable", err.Error())
	}
	for _, file := range files {
		if file.AuthIndex != body.AuthIndex || authCategory(file.Type) != "codex" || file.Disabled || file.ID == "" {
			continue
		}
		c := &a.controls
		c.mu.Lock()
		defer c.mu.Unlock()
		if _, exists := c.active[body.RequestID]; exists {
			return JSONError(409, "duplicate", "Test already reserved")
		}
		if c.settings.MaxConcurrency > 0 && c.counts[file.ID] >= c.settings.MaxConcurrency {
			return JSONError(429, "busy", "Credential is busy")
		}
		c.active[body.RequestID] = file.ID
		c.counts[file.ID]++
		return JSONResponse(200, map[string]bool{"reserved": true})
	}
	return JSONError(404, "not_found", "Enabled Codex credential not found")
}

// Record only status and opaque auth index; never store prompts, response bodies or tokens.
func (a *App) credentialTaskResult(req ManagementRequest) ManagementResponse {
	var body struct {
		AuthIndex string `json:"auth_index"`
		Success   bool   `json:"success"`
		Status    int    `json:"status"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return JSONError(400, "invalid", err.Error())
	}
	a.controls.mu.Lock()
	_, exists := a.controls.active[body.RequestID]
	a.controls.mu.Unlock()
	if !exists || !strings.HasPrefix(body.RequestID, "probe:") {
		return JSONError(400, "invalid", "Unknown test request")
	}
	a.store.AddPluginLog(billing.PluginLogInfo, "Scheduled Codex test auth_index=%q success=%t status=%d", cleanText(body.AuthIndex), body.Success, body.Status)
	return JSONResponse(200, map[string]bool{"recorded": true})
}
