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
	"time"

	"cpa-key-billing-plus/internal/billing"
	"cpa-key-billing-plus/internal/tasksettings"
	"gopkg.in/yaml.v3"
)

const credentialRequestHeader = "X-Cpa-Billing-Plus-Request"

type credentialControls struct {
	mu          sync.Mutex
	settings    tasksettings.Settings
	lastRunAt   time.Time
	nextRunAt   time.Time
	executing   bool
	path        string
	active      map[string]string
	counts      map[string]int
	sessions    map[string]credentialSession
	worker      *os.Process
	origin, key string // Plaintext stays in memory; disk persistence uses the encrypted session store.
	taskError   string
	// sessionAffinity mirrors CPA's routing.session-affinity switch so the
	// plugin only applies its own sticky-session reservation when CPA does.
	sessionAffinity bool
}

func (c *credentialControls) configure(path string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.path == path {
		return nil
	}
	saved := struct {
		tasksettings.Settings
		LastRunAt time.Time `json:"last_run_at"`
	}{Settings: tasksettings.Default()}
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		if err = json.Unmarshal(raw, &saved); err != nil {
			return err
		}
	}
	if err = saved.Settings.Validate(); err != nil {
		return err
	}
	c.stopWorker()
	c.path, c.settings, c.lastRunAt = path, saved.Settings, saved.LastRunAt
	c.taskError = ""
	c.sessions = nil
	if c.active == nil {
		c.active = map[string]string{}
		c.counts = map[string]int{}
	}
	return nil
}

func routingSessionAffinity(raw []byte) bool {
	var document struct {
		Routing struct {
			SessionAffinity bool `yaml:"session-affinity"`
		} `yaml:"routing"`
	}
	if yaml.Unmarshal(raw, &document) != nil {
		return false
	}
	return document.Routing.SessionAffinity
}

// Lifecycle YAML contains plugin settings only. CPA's -config argument
// identifies the host routing policy; the default is config.yaml in its cwd.
func (a *App) refreshSessionAffinity() {
	path := "config.yaml"
	for i, arg := range os.Args[1:] {
		if (arg == "-config" || arg == "--config") && i+2 < len(os.Args) {
			path = os.Args[i+2]
			break
		}
		if strings.HasPrefix(arg, "-config=") || strings.HasPrefix(arg, "--config=") {
			path = strings.SplitN(arg, "=", 2)[1]
			break
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	c := &a.controls
	c.mu.Lock()
	defer c.mu.Unlock()
	enabled := routingSessionAffinity(raw)
	if enabled != c.sessionAffinity {
		c.sessions = nil
		c.sessionAffinity = enabled
	}
}

func (c *credentialControls) stopWorker() {
	c.nextRunAt, c.executing = time.Time{}, false
	if c.worker != nil {
		_ = c.worker.Kill()
		_, _ = c.worker.Wait()
		c.worker = nil
	}
}

// Reap exited workers during host calls; a retained process pointer is not proof of a live timer.
func (c *credentialControls) workerRunning() bool {
	if c.worker != nil && workerExited(c.worker) {
		_ = c.worker.Release()
		c.worker = nil
		c.nextRunAt, c.executing = time.Time{}, false
	}
	return c.worker != nil
}

// The timer lives in a separate executable, keeping the embedded Go runtime idle between host calls.
func (c *credentialControls) startWorker(origin, key string) error {
	if !c.settings.Enabled {
		c.stopWorker()
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
	if c.workerRunning() && c.origin == origin && c.key == key {
		return nil
	}
	worker := filepath.Join("plugins", "cpa-key-billing-plus-worker")
	if runtime.GOOS == "windows" {
		worker += ".exe"
	}
	if _, err := os.Stat(worker); err != nil {
		return fmt.Errorf("install the task worker in plugins before enabling tasks")
	}
	// Keep only authenticated ciphertext on disk; the worker still receives its credential through stdin.
	if err := tasksettings.SaveSession(c.path, tasksettings.Session{Origin: origin, Key: key}); err != nil {
		return fmt.Errorf("save encrypted task session: %w", err)
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
	c.taskError = ""
	return nil
}

// Startup recovery runs synchronously in the host lifecycle, with no plugin-owned timer.
func (c *credentialControls) resumeSavedWorker() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.settings.Enabled || c.workerRunning() {
		return nil
	}
	_, err := os.Stat(filepath.Join(c.path+".task-session", "session.enc"))
	if os.IsNotExist(err) {
		return nil // Existing installations enroll the management session on their next login.
	}
	var session tasksettings.Session
	if err == nil {
		session, err = tasksettings.LoadSession(c.path)
	}
	if err == nil {
		err = c.startWorker(session.Origin, session.Key)
	}
	if err != nil {
		c.taskError = err.Error()
	}
	return err
}

func (a *App) credentialSettings(req ManagementRequest) ManagementResponse {
	a.refreshSessionAffinity()
	c := &a.controls
	// Status polls can recover an exited worker from the encrypted session without a fresh browser login.
	if req.Method == http.MethodGet {
		_ = c.resumeSavedWorker()
	}
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
		if err := c.saveSettings(cfg, c.lastRunAt); err != nil {
			return JSONError(500, "save_failed", err.Error())
		}
		if !sameTaskSchedule(cfg, c.settings) {
			c.nextRunAt, c.executing = time.Time{}, false
		}
		if cfg.SessionTTLMinutes != c.settings.SessionTTLMinutes {
			c.sessions = nil // Existing deadlines must not survive a TTL policy edit.
		}
		c.settings = cfg
		if !cfg.Enabled {
			c.stopWorker()
		}
	}
	// A running worker polls without replacing its session; browser logins may update the saved credential.
	if req.Query.Get("origin") != "" {
		key := strings.TrimPrefix(req.Headers.Get("Authorization"), "Bearer ")
		if err := c.startWorker(req.Query.Get("origin"), key); err != nil {
			c.taskError = err.Error()
			return JSONResponse(200, map[string]any{"settings": c.settings, "running": false, "error": err.Error(), "last_run_at": taskTimestamp(c.lastRunAt), "next_run_at": nil})
		}
	}
	next := c.nextRunAt
	running := c.workerRunning() && c.settings.Enabled
	if !running {
		next = time.Time{}
	}
	return JSONResponse(200, map[string]any{"settings": c.settings, "sticky_sessions": c.sessionAffinity, "running": running, "executing": running && c.executing, "error": c.taskError, "last_run_at": taskTimestamp(c.lastRunAt), "next_run_at": taskTimestamp(next)})
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
		if c.settings.MaxConcurrency > 0 && c.counts[file.ID] >= c.settings.MaxConcurrency-2 {
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

// Persist the last batch start with settings so a host restart retains the history.
func (c *credentialControls) saveSettings(cfg tasksettings.Settings, last time.Time) error {
	raw, _ := json.MarshalIndent(struct {
		tasksettings.Settings
		LastRunAt time.Time `json:"last_run_at,omitempty"`
	}{cfg, last}, "", "  ")
	temp, err := os.CreateTemp(filepath.Dir(c.path), ".plus-settings-*")
	if err != nil {
		return err
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
		return err
	}
	return nil
}

func taskTimestamp(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value.UTC().Format(time.RFC3339)
}

func sameTaskSchedule(a, b tasksettings.Settings) bool {
	// Concurrency and affinity edits do not reset the task deadline.
	a.MaxConcurrency, b.MaxConcurrency = 0, 0
	a.SessionTTLMinutes, b.SessionTTLMinutes = 0, 0
	return a == b
}

// Only the worker's current schedule may publish timing; edits invalidate old reports.
func (a *App) credentialTaskStatus(req ManagementRequest) ManagementResponse {
	var body struct {
		Settings  tasksettings.Settings `json:"settings"`
		Started   bool                  `json:"started"`
		NextRunAt time.Time             `json:"next_run_at"`
	}
	if err := decodeStrict(req.Body, &body); err != nil {
		return errorResponse(err)
	}
	c := &a.controls
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.settings.Enabled || !sameTaskSchedule(body.Settings, c.settings) {
		return JSONError(409, "schedule_changed", "Task schedule changed")
	}
	if body.Started {
		now := time.Now()
		if err := c.saveSettings(c.settings, now); err != nil {
			return JSONError(500, "save_failed", err.Error())
		}
		c.lastRunAt, c.nextRunAt, c.executing = now, time.Time{}, true
	} else {
		if body.NextRunAt.IsZero() {
			return JSONError(400, "invalid", "Next execution time is required")
		}
		c.nextRunAt, c.executing = body.NextRunAt, false
	}
	return JSONResponse(200, map[string]bool{"recorded": true})
}

// Match host lifecycle IDs, including failover, while keeping completion and observation ordered.
func (a *App) observeCredentialConcurrency(requestID, authID string) {
	if requestID == "" || authID == "" {
		return
	}
	a.admissionsMu.Lock()
	defer a.admissionsMu.Unlock()
	if admission := a.admissions[requestID]; admission != nil && admission.completed {
		return
	}
	c := &a.controls
	c.mu.Lock()
	defer c.mu.Unlock()
	previous := c.active[requestID]
	if previous == authID {
		return
	}
	if previous != "" {
		c.counts[previous]--
		if c.counts[previous] <= 0 {
			delete(c.counts, previous)
		}
	}
	c.active[requestID] = authID
	c.counts[authID]++
}
