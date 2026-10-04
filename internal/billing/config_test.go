package billing

import (
	"testing"
	"time"
)

func TestDecodeConfigDefaults(t *testing.T) {
	cfg, errDecode := DecodeConfig([]byte("enabled: true\npriority: 10\nstore:\n  id: cpa-key-billing-plus\n  version: 0.5.1\n"))
	if errDecode != nil {
		t.Fatalf("DecodeConfig: %v", errDecode)
	}
	if !cfg.Enabled || cfg.Debug || cfg.CodexFastModeBilling || cfg.MaskAPIKeyViewEmails || cfg.AllowAPIKeyQuotaReset || cfg.StateFile != DefaultStateFile || cfg.RequestEventRetentionDays != DefaultRequestEventRetentionDays {
		t.Fatalf("config = %+v", cfg)
	}
	cfg, errDecode = DecodeConfig([]byte("enabled: true\ndebug: true\ncodex_fast_mode_billing: true\nmask_api_key_view_emails: true\nallow_api_key_quota_reset: true\n"))
	if errDecode != nil || !cfg.Debug || !cfg.CodexFastModeBilling || !cfg.MaskAPIKeyViewEmails || !cfg.AllowAPIKeyQuotaReset {
		t.Fatalf("config = %+v, error = %v", cfg, errDecode)
	}
}

func TestDecodeConfigUsesRequestEventRetentionDays(t *testing.T) {
	cfg, err := DecodeConfig([]byte("request_event_retention_days: 180\n"))
	if err != nil {
		t.Fatalf("DecodeConfig error = %v", err)
	}
	if cfg.RequestEventRetentionDays != 180 || cfg.RequestEventRetention() != 180*24*time.Hour {
		t.Fatalf("retention = %d days / %s", cfg.RequestEventRetentionDays, cfg.RequestEventRetention())
	}
}

func TestDecodeConfigRejectsExcessiveRequestEventRetention(t *testing.T) {
	if _, err := DecodeConfig([]byte("request_event_retention_days: 36501\n")); err == nil {
		t.Fatal("excessive request event retention was accepted")
	}
}

func TestDecodeConfigRejectsUnknownFieldsAndExtraDocuments(t *testing.T) {
	for name, raw := range map[string]string{
		"unknown field":  "enable: true\n",
		"extra document": "enabled: true\n---\nenabled: false\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, errDecode := DecodeConfig([]byte(raw)); errDecode == nil {
				t.Fatal("DecodeConfig accepted invalid configuration")
			}
		})
	}
}
