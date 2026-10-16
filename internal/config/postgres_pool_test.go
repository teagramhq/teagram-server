package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/teagramhq/teagram-server/internal/config"
)

func TestLoadPostgresPool(t *testing.T) {
	for _, override := range []bool{false, true} {
		t.Run(map[bool]string{false: "defaults", true: "overrides"}[override], func(t *testing.T) {
			t.Setenv("TG_POSTGRES_DSN", "postgres://localhost/tg")
			t.Setenv("TG_AUTHKEY_ENC_KEY", validEncKey)
			t.Setenv("TG_PG_MAX_CONNS", "")
			t.Setenv("TG_PG_MIN_CONNS", "")
			t.Setenv("TG_PG_MAX_CONN_IDLE_TIME", "")
			wantMax, wantMin, wantIdle := int32(8), int32(2), 5*time.Minute
			if override {
				t.Setenv("TG_PG_MAX_CONNS", "12")
				t.Setenv("TG_PG_MIN_CONNS", "0")
				t.Setenv("TG_PG_MAX_CONN_IDLE_TIME", "90s")
				wantMax, wantMin, wantIdle = 12, 0, 90*time.Second
			}
			cfg, err := config.Load(discardLog())
			if err != nil {
				t.Fatal(err)
			}
			if cfg.PostgresPool.MaxConns != wantMax || cfg.PostgresPool.MinConns != wantMin || cfg.PostgresPool.MaxConnIdleTime != wantIdle {
				t.Fatalf("pool = %+v, want max %d min %d idle %s", cfg.PostgresPool, wantMax, wantMin, wantIdle)
			}
		})
	}
}

func TestLoadRejectsInvalidPostgresPool(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"TG_PG_MAX_CONNS", "1"},
		{"TG_PG_MAX_CONNS", "2147483648"},
		{"TG_PG_MAX_CONNS", "many"},
		{"TG_PG_MIN_CONNS", "-1"},
		{"TG_PG_MIN_CONNS", "9"},
		{"TG_PG_MAX_CONN_IDLE_TIME", "0s"},
		{"TG_PG_MAX_CONN_IDLE_TIME", "-1s"},
		{"TG_PG_MAX_CONN_IDLE_TIME", "soon"},
	} {
		t.Run(tc.key+"/"+tc.value, func(t *testing.T) {
			t.Setenv("TG_POSTGRES_DSN", "postgres://localhost/tg")
			t.Setenv("TG_AUTHKEY_ENC_KEY", validEncKey)
			t.Setenv("TG_PG_MAX_CONNS", "8")
			t.Setenv("TG_PG_MIN_CONNS", "2")
			t.Setenv("TG_PG_MAX_CONN_IDLE_TIME", "5m")
			t.Setenv(tc.key, tc.value)
			_, err := config.Load(discardLog())
			if err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("Load error = %v, want rejection naming %s", err, tc.key)
			}
		})
	}
}
