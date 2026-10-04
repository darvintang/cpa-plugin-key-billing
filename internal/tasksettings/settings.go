// Created by Darvin.
package tasksettings

import (
	"fmt"
	"strings"
	"time"
	_ "time/tzdata"
)

// One policy applies to all credentials; no per-file task or concurrency overrides.
type Settings struct {
	SessionTTLMinutes    int    `json:"session_ttl_minutes"`
	IgnoreUnpricedModels bool   `json:"ignore_unpriced_models"`
	MaxConcurrency       int    `json:"max_concurrency"`
	Enabled              bool   `json:"enabled"`
	Mode                 string `json:"mode"`
	IntervalMinutes      int    `json:"interval_minutes"`
	TimeOfDay            string `json:"time_of_day"`
	Timezone             string `json:"timezone"`
	Model                string `json:"model"`
	Prompt               string `json:"prompt"`
}

func Default() Settings {
	return Settings{SessionTTLMinutes: 5, Mode: "interval", IntervalMinutes: 60, TimeOfDay: "09:00", Timezone: "Asia/Shanghai", Prompt: "Reply with OK."}
}

func (s Settings) Validate() error {
	// Two slots are reserved for established sessions; new requests require at least two ordinary slots.
	if s.MaxConcurrency < 0 || s.MaxConcurrency > 1000000 || s.MaxConcurrency > 0 && s.MaxConcurrency < 4 {
		return fmt.Errorf("max_concurrency must be 0 or 4–1000000")
	}
	if s.SessionTTLMinutes < 1 || s.SessionTTLMinutes > 525600 {
		return fmt.Errorf("session_ttl_minutes must be 1–525600")
	}
	if s.Mode != "interval" && s.Mode != "daily" {
		return fmt.Errorf("task mode must be interval or daily")
	}
	if s.IntervalMinutes < 1 || s.IntervalMinutes > 525600 {
		return fmt.Errorf("interval_minutes must be 1–525600")
	}
	if _, err := time.Parse("15:04", s.TimeOfDay); err != nil {
		return fmt.Errorf("daily time must be HH:MM")
	}
	if _, err := time.LoadLocation(s.Timezone); err != nil || s.Timezone == "" {
		return fmt.Errorf("invalid task timezone")
	}
	if s.Enabled && (!strings.HasPrefix(strings.ToLower(strings.TrimSpace(s.Model)), "gpt-") || strings.TrimSpace(s.Prompt) == "") {
		return fmt.Errorf("task model and prompt are required")
	}
	if len(s.Model) > 256 || len(s.Prompt) > 4096 {
		return fmt.Errorf("model or prompt is too long")
	}
	return nil
}

// Daily runs use wall-clock time in the configured zone; missed runs are not replayed.
func (s Settings) Next(now time.Time) time.Time {
	if s.Mode == "interval" {
		return now.Add(time.Duration(s.IntervalMinutes) * time.Minute)
	}
	zone, err := time.LoadLocation(s.Timezone)
	if err != nil {
		return time.Time{}
	}
	clock, err := time.Parse("15:04", s.TimeOfDay)
	if err != nil {
		return time.Time{}
	}
	local := now.In(zone)
	next := time.Date(local.Year(), local.Month(), local.Day(), clock.Hour(), clock.Minute(), 0, 0, zone)
	if !next.After(now) {
		next = time.Date(local.Year(), local.Month(), local.Day()+1, clock.Hour(), clock.Minute(), 0, 0, zone)
	}
	return next
}
