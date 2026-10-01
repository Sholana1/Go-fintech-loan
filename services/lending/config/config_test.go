package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEmbeddedDefaultsAreValid(t *testing.T) {
	prod, pol, err := Load("", "")
	if err != nil {
		t.Fatal(err)
	}
	if prod.ID != "personal-loan" || prod.Version != 1 || pol.Version == "" {
		t.Fatalf("unexpected defaults: %+v %+v", prod, pol)
	}
}

func TestUnknownOrInvalidConfigurationStopsStartup(t *testing.T) {
	dir := t.TempDir()
	typo := filepath.Join(dir, "typo.json")
	// "late_fee_minors" is a typo: it must be an error, not a silent zero fee.
	if err := os.WriteFile(typo, []byte(`{"id":"x","version":1,"late_fee_minors":5}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(typo, ""); err == nil {
		t.Fatal("an unknown key must be rejected")
	}
	invalid := filepath.Join(dir, "invalid.json")
	if err := os.WriteFile(invalid, []byte(`{"id":"x","version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(invalid, ""); err == nil {
		t.Fatal("an incomplete product must be rejected")
	}
	if _, _, err := Load(filepath.Join(dir, "missing.json"), ""); err == nil {
		t.Fatal("a missing file must be an error")
	}
}
