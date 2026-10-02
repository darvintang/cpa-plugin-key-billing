// Created by Darvin.
package main

import (
	"cpa-key-billing-plus/internal/tasksettings"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestProbeResponseCompletion(t *testing.T) {
	for _, tt := range []struct {
		body    string
		success bool
	}{
		{`data: {"type":"response.completed","response":{"status":"completed","error":null}}`, true},
		{`data: {"type":"response.created"}`, false},
		{`data: {"type":"response.failed","error":{"message":"failed"}}`, false},
		{`data: {"type":"response.completed","response":{"status":"incomplete"}}`, false},
		{"event: error\ndata: {\"type\":\"response.completed\"}", false},
		{`data: invalid`, false},
	} {
		if got := probeSucceeded(tt.body); got != tt.success {
			t.Fatalf("body=%q success=%t", tt.body, got)
		}
	}
}

// Test the real management request flow without calling an upstream provider.
func TestCodexProbesReserveReportAndRelease(t *testing.T) {
	slots, releases, probes, reports := 0, 0, 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer dummy-management-session" {
			t.Error("missing management session")
		}
		switch r.URL.Path {
		case "/v0/management/auth-files":
			io.WriteString(w, `{"files":[{"auth_index":"enabled","provider":"codex"},{"auth_index":"disabled","provider":"codex","disabled":true},{"auth_index":"other","provider":"claude"}]}`)
		case base + "/credential-task-slot":
			var body struct{ Release bool }
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Release {
				releases++
			} else {
				slots++
			}
			io.WriteString(w, `{}`)
		case "/v0/management/requests/api-call":
			probes++
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["auth_index"] != "enabled" || body["url"] != "https://chatgpt.com/backend-api/codex/responses" {
				t.Error(body)
			}
			header := body["header"].(map[string]any)
			if header["Authorization"] != "Bearer $TOKEN$" {
				t.Error("host token substitution was lost")
			}
			json.NewEncoder(w).Encode(map[string]any{"status_code": 200, "body": `data: {"type":"response.completed","response":{"status":"completed"}}`})
		case base + "/credential-task-result":
			reports++
			var body struct{ Success bool }
			_ = json.NewDecoder(r.Body).Decode(&body)
			if !body.Success {
				t.Error("completed response was reported as failed")
			}
			io.WriteString(w, `{}`)
		default:
			t.Error(r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	cfg := tasksettings.Default()
	cfg.Model = "gpt-test"
	session{Origin: server.URL, Key: "dummy-management-session", Parent: os.Getppid()}.probeAll(cfg)
	if slots != 1 || releases != 1 || probes != 1 || reports != 1 {
		t.Fatalf("slots=%d releases=%d probes=%d reports=%d", slots, releases, probes, reports)
	}
}
