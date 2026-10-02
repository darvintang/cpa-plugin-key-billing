// Created by Darvin.
package plugin

import (
	"cpa-key-billing-plus/internal/tasksettings"
	"encoding/json"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"cpa-key-billing-plus/internal/billing"
)

// Concurrent keys share the same credential ceiling; completion releases the exact reservation.
func TestCredentialConcurrencyAcrossKeys(t *testing.T) {
	app := newConfiguredApp(t)
	app.controls.settings.MaxConcurrency = 2
	for i := 0; i < 24; i++ {
		app.controls.active[string(rune('a'+i))] = ""
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	selected := []string{}
	for i := 0; i < 24; i++ {
		id := string(rune('a' + i))
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := SchedulerPickRequest{Candidates: []SchedulerAuthCandidate{{ID: "a"}, {ID: "b"}}}
			req.Options.Headers = http.Header{credentialRequestHeader: []string{id}}
			raw, err := app.pickCredentialWithLimit(req, id, billing.RoutingDecision{}, req.Candidates)
			if err != nil {
				t.Error(err)
				return
			}
			var envelope Envelope
			if json.Unmarshal(raw, &envelope) != nil {
				t.Error("bad envelope")
				return
			}
			if envelope.OK {
				mu.Lock()
				selected = append(selected, id)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(selected) != 4 || app.controls.counts["a"] != 2 || app.controls.counts["b"] != 2 {
		t.Fatalf("selected=%d counts=%v", len(selected), app.controls.counts)
	}
	for _, id := range selected {
		app.releaseCredential(id)
		app.releaseCredential(id)
	}
	if len(app.controls.counts) != 0 {
		t.Fatal(app.controls.counts)
	}
}

func TestSharedSettingsPersistAndRejectInvalidEdits(t *testing.T) {
	app := newConfiguredApp(t)
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := app.controls.configure(path); err != nil {
		t.Fatal(err)
	}
	cfg := app.controls.settings
	cfg.MaxConcurrency = 3
	raw, _ := json.Marshal(cfg)
	if result := app.credentialSettings(ManagementRequest{Method: http.MethodPut, Body: raw}); result.StatusCode != 200 {
		t.Fatal(result)
	}
	var fresh credentialControls
	if err := fresh.configure(path); err != nil || fresh.settings.MaxConcurrency != 3 {
		t.Fatalf("config=%+v err=%v", fresh.settings, err)
	}
	cfg.MaxConcurrency = -1
	raw, _ = json.Marshal(cfg)
	if result := app.credentialSettings(ManagementRequest{Method: http.MethodPut, Body: raw}); result.StatusCode != 400 || app.controls.settings.MaxConcurrency != 3 {
		t.Fatal(result)
	}
}

// The admission header reaches scheduler.pick and is cleared before the upstream sees it.
func TestCredentialReservationLifecycle(t *testing.T) {
	app := newAppWithPrice(t, true)
	app.controls.settings.MaxConcurrency = 1
	req := RequestInterceptRequest{RequestID: "lifecycle-1", Model: "gpt-5.5", SourceFormat: "openai", Metadata: map[string]any{MetadataCallerScope: billing.CallerScope(testAPIKey)}}
	raw, err := app.HandleMethod(MethodRequestInterceptBefore, mustMarshal(t, req))
	if err != nil {
		t.Fatal(err)
	}
	var before RequestInterceptResponse
	decodeResult(t, raw, &before)
	if before.Terminate || before.Headers.Get(credentialRequestHeader) != req.RequestID {
		t.Fatal(before)
	}
	pick := SchedulerPickRequest{Model: req.Model, Candidates: []SchedulerAuthCandidate{{ID: "only", Metadata: map[string]any{"weight": 2}}}}
	pick.Options.Headers = before.Headers
	pick.Options.Metadata = req.Metadata
	raw, err = app.HandleMethod(MethodSchedulerPick, mustMarshal(t, pick))
	if err != nil {
		t.Fatal(err)
	}
	var selected SchedulerPickResponse
	decodeResult(t, raw, &selected)
	if !selected.Handled || selected.AuthID != "only" {
		t.Fatal(selected)
	}
	raw, err = app.HandleMethod(MethodRequestInterceptAfter, mustMarshal(t, req))
	if err != nil {
		t.Fatal(err)
	}
	var after RequestInterceptResponse
	decodeResult(t, raw, &after)
	if len(after.ClearHeaders) != 1 || after.ClearHeaders[0] != credentialRequestHeader {
		t.Fatal(after)
	}
	_, err = app.HandleMethod(MethodRequestComplete, mustMarshal(t, RequestCompletion{RequestID: req.RequestID}))
	if err != nil {
		t.Fatal(err)
	}
	if len(app.controls.active) != 0 || len(app.controls.counts) != 0 {
		t.Fatal(app.controls.active, app.controls.counts)
	}
	app.controls.active["blocked"] = ""
	app.releaseCredential("blocked")
	if len(app.controls.active) != 0 {
		t.Fatal("unselected request leaked a reservation")
	}
}

// Runtime timing survives restart only for the past run; stale schedules cannot overwrite it.
func TestCredentialTaskTiming(t *testing.T) {
	app := newConfiguredApp(t)
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := app.controls.configure(path); err != nil {
		t.Fatal(err)
	}
	cfg := app.controls.settings
	cfg.Enabled, cfg.Model = true, "gpt-test"
	app.controls.settings = cfg
	next := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	report := func(started bool, settings tasksettings.Settings) ManagementResponse {
		return app.credentialTaskStatus(ManagementRequest{Body: mustMarshal(t, map[string]any{"settings": settings, "started": started, "next_run_at": next})})
	}
	if r := report(false, cfg); r.StatusCode != 200 || !app.controls.nextRunAt.Equal(next) {
		t.Fatal(r)
	}
	if r := report(true, cfg); r.StatusCode != 200 || app.controls.lastRunAt.IsZero() || !app.controls.executing || !app.controls.nextRunAt.IsZero() {
		t.Fatal(r)
	}
	last := app.controls.lastRunAt
	if r := report(false, cfg); r.StatusCode != 200 || app.controls.executing {
		t.Fatal(r)
	}
	stale := cfg
	stale.IntervalMinutes++
	if r := report(true, stale); r.StatusCode != 409 || !app.controls.lastRunAt.Equal(last) {
		t.Fatal(r)
	}
	var fresh credentialControls
	if err := fresh.configure(path); err != nil || !fresh.lastRunAt.Equal(last) || !fresh.nextRunAt.IsZero() {
		t.Fatalf("restart: %+v %v", fresh, err)
	}
}

// Unlimited requests, duplicate observations and failover share the admission counters.
func TestCredentialConcurrencyDisplay(t *testing.T) {
	app := newConfiguredApp(t)
	app.SetHostCaller(func(method string, _ any) (json.RawMessage, error) {
		return json.RawMessage(`{"files":[{"id":"a","auth_index":"index-a","type":"codex","name":"a.json"},{"id":"b","auth_index":"index-b","type":"codex","name":"b.json"}]}`), nil
	})
	app.observeCredentialConcurrency("one", "a")
	app.observeCredentialConcurrency("one", "a")
	app.observeCredentialConcurrency("two", "a")
	files, err := app.listAuthFiles(viewAccess{})
	if err != nil || *files[0].CurrentConcurrency != 2 || *files[0].MaxConcurrency != 0 {
		t.Fatalf("files=%+v err=%v", files, err)
	}
	app.observeCredentialConcurrency("one", "b")
	if app.controls.counts["a"] != 1 || app.controls.counts["b"] != 1 {
		t.Fatal(app.controls.counts)
	}
	app.releaseCredential("one")
	app.releaseCredential("two")
	if len(app.controls.counts) != 0 {
		t.Fatal(app.controls.counts)
	}
}
