package config_test

// CONTRACT-23 T1 — the configuration matrix of the vector capability, in the same
// shape CONTRACT-21 fixed the engine matrix: named values, an explicit default,
// and an unrecognised value that is a startup error rather than a fallback.

import (
	"strings"
	"testing"

	"github.com/MauricioPerera/librarian/internal/config"
	"github.com/MauricioPerera/librarian/internal/store"
)

func TestResolveCapabilities(t *testing.T) {
	cases := []struct {
		name        string
		value       string
		wantVector  bool
		wantErrPart string
	}{
		// The DEFAULT, and the reason it is what it is: an installation that
		// says nothing keeps the schema it already has.
		{name: "unset defaults to enabled", value: "", wantVector: true},
		{name: "enabled", value: "enabled", wantVector: true},
		{name: "on", value: "on", wantVector: true},
		{name: "true", value: "true", wantVector: true},
		{name: "1", value: "1", wantVector: true},
		{name: "case insensitive", value: "EnAbLeD", wantVector: true},
		{name: "padded", value: "  disabled  ", wantVector: false},
		{name: "disabled", value: "disabled", wantVector: false},
		{name: "off", value: "off", wantVector: false},
		{name: "false", value: "false", wantVector: false},
		{name: "0", value: "0", wantVector: false},
		// A typo must never be read as "disabled": that would silently drop a
		// column from a fresh installation, and the choice is irreversible.
		{name: "typo is refused", value: "disbled", wantErrPart: "is not a valid value"},
		{name: "no is refused", value: "no", wantErrPart: "IRREVERSIBLE"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(config.VectorVar, tc.value)
			caps, err := config.ResolveCapabilities()
			if tc.wantErrPart != "" {
				if err == nil {
					t.Fatalf("%q was accepted (vector=%v); want an error containing %q", tc.value, caps.Vector(), tc.wantErrPart)
				}
				if !strings.Contains(err.Error(), tc.wantErrPart) {
					t.Fatalf("error = %q, want it to contain %q", err, tc.wantErrPart)
				}
				t.Logf("REFUSED as required: %v", err)
				return
			}
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if caps.Vector() != tc.wantVector {
				t.Fatalf("%q → vector=%v, want %v", tc.value, caps.Vector(), tc.wantVector)
			}
		})
	}
}

// TestLoadCarriesTheCapabilities checks the whole Config, since that is what the
// startup path actually reads.
func TestLoadCarriesTheCapabilities(t *testing.T) {
	t.Setenv("LIBRARIAN_JWT_SECRET", "secret")
	t.Setenv(config.EngineVar, "")
	t.Setenv(config.DSNVar, "")

	t.Setenv(config.VectorVar, "")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Capabilities.Vector() || cfg.Capabilities.Describe() != "enabled" {
		t.Fatalf("default Config capabilities = %v, want enabled", cfg.Capabilities)
	}

	t.Setenv(config.VectorVar, "disabled")
	cfg, err = config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Capabilities.Vector() || cfg.Capabilities.Describe() != "disabled" {
		t.Fatalf("Config capabilities = %v, want disabled", cfg.Capabilities)
	}

	t.Setenv(config.VectorVar, "nonsense")
	if _, err := config.Load(); err == nil {
		t.Fatal("Load accepted an invalid LIBRARIAN_VECTOR")
	}
}

// TestStoreNamesTheSameVariable pins the one string internal/store repeats rather
// than imports (it sits below the environment layer): the coherence guard's
// message tells the operator which knob to turn, so it must be the real one.
func TestStoreNamesTheSameVariable(t *testing.T) {
	if store.VectorVarName != config.VectorVar {
		t.Fatalf("store.VectorVarName = %q but config.VectorVar = %q", store.VectorVarName, config.VectorVar)
	}
}
