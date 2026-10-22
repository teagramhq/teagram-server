package config_test

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/teagramhq/teagram-server/internal/config"
)

func TestSessionCollectionConfigDefaultsAndBooleanValues(t *testing.T) {
	t.Setenv("TG_POSTGRES_DSN", "postgres://localhost/tg")
	t.Setenv("TG_AUTHKEY_ENC_KEY", validEncKey)
	t.Setenv("TG_SESSION_IP", "")
	t.Setenv("TG_SESSION_DEVICE_LABELS", "")

	cfg, err := config.Load(slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Load with session settings unset: %v", err)
	}
	assertSessionCollectionConfig(t, cfg, false, true)

	for _, tc := range []struct {
		name       string
		ip         string
		labels     string
		wantIP     bool
		wantLabels bool
	}{
		{name: "both enabled", ip: "true", labels: "true", wantIP: true, wantLabels: true},
		{name: "both disabled", ip: "false", labels: "false"},
		{name: "IP only", ip: "true", labels: "false", wantIP: true},
		{name: "labels only", ip: "false", labels: "true", wantLabels: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TG_SESSION_IP", tc.ip)
			t.Setenv("TG_SESSION_DEVICE_LABELS", tc.labels)
			cfg, err := config.Load(slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			assertSessionCollectionConfig(t, cfg, tc.wantIP, tc.wantLabels)
		})
	}
}

func TestSessionCollectionConfigRejectsInvalidBooleanValues(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
		bad  string
	}{
		{name: "IP maybe", env: "TG_SESSION_IP", bad: "maybe"},
		{name: "IP padded on", env: "TG_SESSION_IP", bad: "on "},
		{name: "labels maybe", env: "TG_SESSION_DEVICE_LABELS", bad: "maybe"},
		{name: "labels padded on", env: "TG_SESSION_DEVICE_LABELS", bad: "on "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TG_POSTGRES_DSN", "postgres://localhost/tg")
			t.Setenv("TG_AUTHKEY_ENC_KEY", validEncKey)
			t.Setenv("TG_SESSION_IP", "")
			t.Setenv("TG_SESSION_DEVICE_LABELS", "")
			t.Setenv(tc.env, tc.bad)

			_, err := config.Load(slog.New(slog.DiscardHandler))
			if err == nil || !strings.Contains(err.Error(), tc.env) {
				t.Fatalf("Load error = %v, want an error naming %s", err, tc.env)
			}
		})
	}
}

func assertSessionCollectionConfig(t *testing.T, cfg config.Config, wantIP, wantLabels bool) {
	t.Helper()
	if cfg.SessionIP != wantIP {
		t.Errorf("Config.SessionIP = %t, want %t", cfg.SessionIP, wantIP)
	}
	if cfg.SessionDeviceLabels != wantLabels {
		t.Errorf("Config.SessionDeviceLabels = %t, want %t", cfg.SessionDeviceLabels, wantLabels)
	}
}
