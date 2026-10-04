// Created by Darvin.
// This external process owns timers; the shared-library plugin has no background Go activity.
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"cpa-key-billing-plus/internal/tasksettings"
)

const base = "/v0/management/plugins/cpa-key-billing-plus"

var client = &http.Client{Timeout: 70 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

type session struct {
	Origin, Key string
	Parent      int
}

func (s session) call(method, path string, body any) ([]byte, int, error) {
	var data []byte
	if body != nil {
		data, _ = json.Marshal(body)
	}
	req, err := http.NewRequest(method, s.Origin+path, bytes.NewReader(data))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+s.Key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return raw, resp.StatusCode, err
}

func main() {
	var s session
	if json.NewDecoder(io.LimitReader(os.Stdin, 16384)).Decode(&s) != nil || s.Key == "" {
		return
	}
	var previous tasksettings.Settings
	var next time.Time
	// Poll configuration without exposing the management session in argv, environment, files or logs.
	for os.Getppid() == s.Parent {
		raw, status, err := s.call("GET", base+"/credential-settings", nil)
		if status == 401 || status == 403 {
			return
		}
		var response struct {
			Settings  tasksettings.Settings `json:"settings"`
			LastRunAt time.Time             `json:"last_run_at"`
		}
		if err == nil && status == 200 && json.Unmarshal(raw, &response) == nil && response.Settings.Validate() == nil {
			cfg := response.Settings
			schedule := cfg
			schedule.MaxConcurrency = 0
			schedule.SessionTTLMinutes = 0        // Affinity edits do not change the timer.
			schedule.IgnoreUnpricedModels = false // Price policy edits do not change the timer.
			if schedule != previous {
				initial := previous.Mode == ""
				previous = schedule
				next = cfg.Next(time.Now())
				// Restart preserves the last interval deadline; a live schedule edit starts a new interval.
				if initial {
					next = nextRun(cfg, response.LastRunAt, time.Now())
				}
			}
			if cfg.Enabled && !time.Now().Before(next) {
				started := time.Now()
				if s.reportSchedule(cfg, true, time.Time{}) {
					s.probeAll(cfg)
					next = nextRun(cfg, started, time.Now())
				}
			}
			if cfg.Enabled {
				s.reportSchedule(cfg, false, next)
			}
		}
		time.Sleep(time.Second * 5)
	}
}

// Intervals count from batch start. Overdue work runs once without overlapping or replaying missed batches.
func nextRun(cfg tasksettings.Settings, lastRun, now time.Time) time.Time {
	if cfg.Mode == "interval" && !lastRun.IsZero() {
		next := cfg.Next(lastRun)
		if next.Before(now) {
			return now
		}
		return next
	}
	return cfg.Next(now)
}

// Report the exact worker deadline rather than recalculating it in the browser.
func (s session) reportSchedule(cfg tasksettings.Settings, started bool, next time.Time) bool {
	_, status, err := s.call("POST", base+"/credential-task-status", map[string]any{"settings": cfg, "started": started, "next_run_at": next})
	return err == nil && status == http.StatusOK
}

func (s session) probeAll(cfg tasksettings.Settings) {
	raw, status, err := s.call("GET", "/v0/management/auth-files", nil)
	if err != nil || status != 200 {
		return
	}
	var response struct {
		Files []struct {
			AuthIndex      string `json:"auth_index"`
			Type, Provider string
			Disabled       bool
			Status         string
		} `json:"files"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return
	}
	for _, file := range response.Files {
		if os.Getppid() != s.Parent {
			return
		}
		provider := file.Provider
		if provider == "" {
			provider = file.Type
		}
		if !strings.EqualFold(provider, "codex") || file.Disabled || file.Status == "disabled" || file.AuthIndex == "" {
			continue
		}
		s.probe(file.AuthIndex, cfg)
	}
}

func (s session) probe(index string, cfg tasksettings.Settings) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return
	}
	id := "probe:" + hex.EncodeToString(nonce)
	_, status, err := s.call("POST", base+"/credential-task-slot", map[string]any{"auth_index": index, "request_id": id})
	if err != nil || status != 200 {
		return
	}
	defer s.call("POST", base+"/credential-task-slot", map[string]any{"request_id": id, "release": true})
	// CPA resolves $TOKEN$ for this exact credential; no upstream key leaves the host.
	body, _ := json.Marshal(map[string]any{"model": cfg.Model, "instructions": "Reply briefly.", "store": false, "stream": true, "input": []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]string{"type": "input_text", "text": cfg.Prompt}}}}})
	payload := map[string]any{"auth_index": index, "method": "POST", "url": "https://chatgpt.com/backend-api/codex/responses", "header": map[string]string{"Authorization": "Bearer $TOKEN$", "Content-Type": "application/json", "Accept": "text/event-stream"}, "data": string(body)}
	raw, status, err := s.call("POST", "/v0/management/requests/api-call", payload)
	if status == 404 {
		raw, status, err = s.call("POST", "/v0/management/api-call", payload)
	}
	// api-call wraps upstream responses; never treat HTTP 200 alone as a successful test.
	var result struct {
		StatusCode int    `json:"status_code"`
		Body       string `json:"body"`
	}
	success := err == nil && status == 200 && json.Unmarshal(raw, &result) == nil && result.StatusCode >= 200 && result.StatusCode < 300 && probeSucceeded(result.Body)
	_, _, _ = s.call("POST", base+"/credential-task-result", map[string]any{"auth_index": index, "request_id": id, "success": success, "status": result.StatusCode})
}

// SSE requires an explicit completion event; error payloads and truncated streams are failures.
func probeSucceeded(body string) bool {
	completed := false
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "event: error" {
			return false
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			continue
		}
		var event struct {
			Type     string          `json:"type"`
			Error    json.RawMessage `json:"error"`
			Response struct {
				Status string          `json:"status"`
				Error  json.RawMessage `json:"error"`
			} `json:"response"`
		}
		if json.Unmarshal([]byte(data), &event) != nil {
			return false
		}
		if event.Type == "error" || event.Type == "response.failed" || event.Type == "response.incomplete" || len(event.Error) > 0 && string(event.Error) != "null" || len(event.Response.Error) > 0 && string(event.Response.Error) != "null" {
			return false
		}
		if event.Type == "response.completed" && (event.Response.Status == "" || event.Response.Status == "completed") {
			completed = true
		}
	}
	return completed
}
