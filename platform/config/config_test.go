package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoaderReportsEveryProblem(t *testing.T) {
	l := FromMap(map[string]string{
		"APP_ENV": "prod",
		"PORT":    "abc",
		"TIMEOUT": "-1s",
		"FLAG":    "maybe",
	})
	_ = l.Environment()
	_ = l.String("DATABASE_URL")
	_ = l.IntOr("PORT", 8080, 1, 65535)
	_ = l.DurationOr("TIMEOUT", time.Second)
	_ = l.BoolOr("FLAG", false)

	err := l.Err()
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, key := range []string{"APP_ENV", "DATABASE_URL", "PORT", "TIMEOUT", "FLAG"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error should mention %s: %v", key, err)
		}
	}
}

func TestLoaderDefaultsAndValues(t *testing.T) {
	l := FromMap(map[string]string{"APP_ENV": "local", "N": "7", "D": "250ms", "B": "true", "S": "x"})
	if l.Environment() != Local || !Local.AllowsSimulators() || Production.AllowsSimulators() || Staging.AllowsSimulators() {
		t.Fatal("environment handling wrong")
	}
	if l.IntOr("N", 1, 0, 10) != 7 || l.IntOr("MISSING", 3, 0, 10) != 3 {
		t.Fatal("IntOr wrong")
	}
	if l.DurationOr("D", time.Second) != 250*time.Millisecond {
		t.Fatal("DurationOr wrong")
	}
	if !l.BoolOr("B", false) || l.String("S") != "x" || l.StringOr("MISSING", "d") != "d" {
		t.Fatal("Bool/String wrong")
	}
	if err := l.Err(); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
}
