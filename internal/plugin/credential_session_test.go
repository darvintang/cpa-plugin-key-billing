// Created by Darvin.
package plugin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"cpa-key-billing-plus/internal/billing"
	"cpa-key-billing-plus/internal/tasksettings"
)

// Exercise real scheduler envelopes and reservations, rather than a parallel cache implementation.
func sessionPick(t *testing.T, app *App, requestID, session, scope string, candidates ...SchedulerAuthCandidate) (SchedulerPickResponse, bool) {
	t.Helper()
	app.controls.active[requestID] = ""
	req := SchedulerPickRequest{Model: "gpt-test", Provider: "codex", Candidates: candidates}
	req.Options.Headers = http.Header{credentialRequestHeader: []string{requestID}}
	req.Options.Metadata = map[string]any{"canonical_session_id": session}
	raw, err := app.pickCredentialWithLimit(req, scope, billing.RoutingDecision{}, candidates)
	if err != nil {
		t.Fatal(err)
	}
	var envelope Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	var result SchedulerPickResponse
	if envelope.OK {
		decodeResult(t, raw, &result)
	}
	return result, envelope.OK
}

func TestStickyReservedCapacityAndExpiry(t *testing.T) {
	app := newConfiguredApp(t)
	app.controls.settings.MaxConcurrency = 4
	candidate := SchedulerAuthCandidate{ID: "only", Provider: "codex"}
	for _, test := range []struct {
		id, session string
		allowed     bool
	}{
		{"cold-1", "s1", true}, {"cold-2", "s2", true}, {"cold-3", "s3", false},
		{"sticky-1", "s1", true}, {"sticky-2", "s1", true}, {"sticky-full", "s1", false},
	} {
		_, ok := sessionPick(t, app, test.id, test.session, "caller", candidate)
		if ok != test.allowed {
			t.Fatalf("%s: allowed=%t", test.id, ok)
		}
	}
	if app.controls.counts["only"] != 4 {
		t.Fatal(app.controls.counts)
	}
	// A denied request must not refresh the session's deadline or borrow capacity from another credential.
	var key string
	for k, binding := range app.controls.sessions {
		if binding.AuthID == "only" {
			key = k
			break
		}
	}
	app.controls.sessions[key] = credentialSession{AuthID: "only", Expires: time.Now().Add(-time.Second)}
	app.releaseCredential("sticky-1")
	app.releaseCredential("sticky-2")
	for k, b := range app.controls.sessions {
		b.Expires = time.Now().Add(-time.Second)
		app.controls.sessions[k] = b
	}
	if _, ok := sessionPick(t, app, "expired", "s1", "caller", candidate); ok {
		t.Fatal("expired binding used reserved capacity")
	}
	app.releaseCredential("cold-1")
	if _, ok := sessionPick(t, app, "renew", "s1", "caller", candidate); !ok {
		t.Fatal("expired binding did not admit after ordinary capacity freed")
	}
	req := SchedulerPickRequest{Model: "gpt-test", Provider: "codex"}
	req.Options.Metadata = map[string]any{"canonical_session_id": "s1"}
	if !app.controls.sessions[credentialSessionKey(req, "caller")].Expires.After(time.Now().Add(4 * time.Minute)) {
		t.Fatal("successful admission did not refresh TTL")
	}
}

func TestStickyPriorityInvalidationAndUnlimited(t *testing.T) {
	app := newConfiguredApp(t)
	app.controls.settings.MaxConcurrency = 4
	low := SchedulerAuthCandidate{ID: "low", Provider: "codex"}
	high := SchedulerAuthCandidate{ID: "high", Provider: "codex", Priority: 10}
	result, ok := sessionPick(t, app, "bind", "s", "caller", low)
	if !ok || result.AuthID != "low" {
		t.Fatal(result, ok)
	}
	result, ok = sessionPick(t, app, "sticky", "s", "caller", low, high)
	if !ok || result.AuthID != "low" {
		t.Fatal("binding lost across priorities", result)
	}
	result, ok = sessionPick(t, app, "new", "new", "caller", low, high)
	if !ok || result.AuthID != "high" {
		t.Fatal("ordinary request ignored priority", result)
	}
	// Removed, route-filtered or zero-weight credentials cannot retain their bindings.
	low.Attributes = map[string]string{"weight": "0"}
	result, ok = sessionPick(t, app, "invalid", "s", "caller", low, high)
	if !ok || result.AuthID != "high" {
		t.Fatal("invalid binding retained", result, ok)
	}
	app.controls.settings.MaxConcurrency = 0
	for i := 0; i < 8; i++ {
		result, ok = sessionPick(t, app, fmt.Sprint("unlimited", i), "s", "caller", high)
		if !ok || result.AuthID != "high" {
			t.Fatal(result, ok)
		}
	}
	result, ok = sessionPick(t, app, "removed", "s", "caller", SchedulerAuthCandidate{ID: "replacement", Provider: "codex"})
	if !ok || result.AuthID != "replacement" {
		t.Fatal(result, ok)
	}
}

func TestSessionIdentityIsolationAndBoundedCache(t *testing.T) {
	req := SchedulerPickRequest{Provider: "codex", Model: "gpt-test"}
	req.Options.Metadata = map[string]any{"canonical_session_id": "same"}
	base := credentialSessionKey(req, "caller")
	if base == "" || credentialSessionKey(req, "") != "" {
		t.Fatal("unsafe caller identity")
	}
	if credentialSessionKey(req, "other") == base {
		t.Fatal("caller collision")
	}
	req.Model = "other"
	if credentialSessionKey(req, "caller") == base {
		t.Fatal("model collision")
	}
	req.Model = "gpt-test"
	req.Provider = "other"
	if credentialSessionKey(req, "caller") == base {
		t.Fatal("provider collision")
	}
	req.Options.Metadata = nil
	req.Options.Headers = http.Header{}
	if credentialSessionKey(req, "caller") != "" {
		t.Fatal("invented a session")
	}
	req.Options.Headers.Set("Session-Id", "legacy")
	if credentialSessionKey(req, "caller") == "" {
		t.Fatal("legacy explicit session ignored")
	}
	c := credentialControls{}
	now := time.Now()
	for i := 0; i < 8200; i++ {
		c.bindSession(fmt.Sprint(i), "auth", now, time.Minute)
	}
	if len(c.sessions) != 8192 {
		t.Fatal(len(c.sessions))
	}
	c.bindSession("refresh", "auth", now.Add(2*time.Minute), time.Minute)
	if len(c.sessions) != 1 {
		t.Fatal("expired entries retained at capacity", len(c.sessions))
	}
}

func TestLegacyCeilingMigrationAndPolicyValidation(t *testing.T) {
	for _, limit := range []int{1, 2, 3} {
		cfg := tasksettings.Default()
		cfg.MaxConcurrency = limit
		path := filepath.Join(t.TempDir(), "settings.json")
		raw, _ := json.Marshal(cfg)
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		c := credentialControls{}
		if err := c.configure(path); err != nil || c.settings.MaxConcurrency != 4 {
			t.Fatal(c.settings, err)
		}
		raw, _ = os.ReadFile(path)
		if err := json.Unmarshal(raw, &cfg); err != nil || cfg.MaxConcurrency != 4 {
			t.Fatal(cfg, err)
		}
	}
	for _, limit := range []int{-1, 1, 2, 3, 1000001} {
		cfg := tasksettings.Default()
		cfg.MaxConcurrency = limit
		if cfg.Validate() == nil {
			t.Fatal("accepted", limit)
		}
	}
	cfg := tasksettings.Default()
	other := cfg
	other.SessionTTLMinutes = 10
	if !sameTaskSchedule(cfg, other) {
		t.Fatal("TTL changed task schedule")
	}
}

// Simultaneous first requests share one binding and may never overflow its total ceiling.
func TestStickyConcurrentFirstBinding(t *testing.T) {
	app := newConfiguredApp(t)
	app.controls.settings.MaxConcurrency = 4
	for i := 0; i < 24; i++ {
		app.controls.active[fmt.Sprint(i)] = ""
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := 0
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := SchedulerPickRequest{Model: "gpt-test", Provider: "codex", Candidates: []SchedulerAuthCandidate{{ID: "a"}, {ID: "b"}}}
			req.Options.Metadata = map[string]any{"canonical_session_id": "same"}
			req.Options.Headers = http.Header{credentialRequestHeader: []string{fmt.Sprint(i)}}
			raw, err := app.pickCredentialWithLimit(req, "caller", billing.RoutingDecision{}, req.Candidates)
			if err != nil {
				t.Error(err)
				return
			}
			var envelope Envelope
			if err := json.Unmarshal(raw, &envelope); err != nil {
				t.Error(err)
				return
			}
			if envelope.OK {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if admitted != 4 || len(app.controls.counts) != 1 || len(app.controls.sessions) != 1 {
		t.Fatal(admitted, app.controls.counts, app.controls.sessions)
	}
}
