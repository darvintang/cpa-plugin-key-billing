package billing

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const DefaultStateFile = "plugins/cpa-key-billing-plus-state-v1.db"

const (
	DefaultRequestEventRetentionDays = 365
	MaxRequestEventRetentionDays     = 36500
)

type Config struct {
	Enabled                   bool   `yaml:"enabled"`
	Debug                     bool   `yaml:"debug"`
	StateFile                 string `yaml:"state_file"`
	CodexFastModeBilling      bool   `yaml:"codex_fast_mode_billing"`
	MaskAPIKeyViewEmails      bool   `yaml:"mask_api_key_view_emails"`
	AllowAPIKeyQuotaReset     bool   `yaml:"allow_api_key_quota_reset"`
	RequestEventRetentionDays int    `yaml:"request_event_retention_days"`
}

func DefaultConfig() Config {
	return Config{
		Enabled:                   false,
		StateFile:                 DefaultStateFile,
		RequestEventRetentionDays: DefaultRequestEventRetentionDays,
	}
}

func DecodeConfig(raw []byte) (Config, error) {
	cfg := DefaultConfig()
	if len(bytes.TrimSpace(raw)) > 0 {
		document := struct {
			Config `yaml:",inline"`
			// These fields belong to the host and are ignored by the plugin.
			Priority int       `yaml:"priority"`
			Store    yaml.Node `yaml:"store"`
		}{Config: cfg}
		decoder := yaml.NewDecoder(bytes.NewReader(raw))
		decoder.KnownFields(true)
		if errDecode := decoder.Decode(&document); errDecode != nil {
			return Config{}, fmt.Errorf("Parse plugin configuration: %w", errDecode)
		}
		if errTrailing := decoder.Decode(&struct{}{}); errTrailing != io.EOF {
			return Config{}, fmt.Errorf("Plugin configuration must contain exactly one YAML document")
		}
		cfg = document.Config
	}
	cfg = cfg.normalized()
	if cfg.RequestEventRetentionDays > MaxRequestEventRetentionDays {
		return Config{}, fmt.Errorf("request_event_retention_days must be between 1 and %d", MaxRequestEventRetentionDays)
	}
	return cfg, nil
}

func (c Config) describe() string {
	if c.Enabled {
		return "enabled"
	}
	return "disabled"
}

func (c Config) normalized() Config {
	c.StateFile = strings.TrimSpace(c.StateFile)
	if c.StateFile == "" {
		c.StateFile = DefaultStateFile
	}
	if c.RequestEventRetentionDays <= 0 {
		c.RequestEventRetentionDays = DefaultRequestEventRetentionDays
	}
	return c
}

// RequestEventRetention keeps loading, querying and cleanup on one retention policy.
func (c Config) RequestEventRetention() time.Duration {
	days := c.RequestEventRetentionDays
	if days <= 0 {
		days = DefaultRequestEventRetentionDays
	}
	if days > MaxRequestEventRetentionDays {
		days = MaxRequestEventRetentionDays
	}
	return time.Duration(days) * 24 * time.Hour
}
