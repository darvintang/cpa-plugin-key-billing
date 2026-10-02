// Created by Darvin.
package tasksettings

import (
	"testing"
	"time"
)

// Check interval and daily boundaries, plus the configuration trust boundary.
func TestSchedule(t *testing.T) {
	now := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	cfg := Default()
	cfg.IntervalMinutes = 17
	if got := cfg.Next(now); !got.Equal(now.Add(17 * time.Minute)) {
		t.Fatal(got)
	}
	cfg.Mode = "daily"
	if got := cfg.Next(now); !got.Equal(time.Date(2026, 10, 2, 1, 0, 0, 0, time.UTC)) {
		t.Fatal(got)
	}
	if got := cfg.Next(now.Add(time.Hour)); !got.Equal(time.Date(2026, 10, 3, 1, 0, 0, 0, time.UTC)) {
		t.Fatal(got)
	}
	cfg.Enabled = true
	cfg.Model = "gpt-test"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Settings){func(s *Settings) { s.IntervalMinutes = 0 }, func(s *Settings) { s.TimeOfDay = "25:00" }, func(s *Settings) { s.Timezone = "bad" }, func(s *Settings) { s.MaxConcurrency = -1 }, func(s *Settings) { s.Model = "claude-test" }} {
		bad := cfg
		change(&bad)
		if bad.Validate() == nil {
			t.Fatal(bad)
		}
	}
}
