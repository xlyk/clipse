package config_test

import (
	"os"
	"reflect"
	"testing"

	"github.com/xlyk/clipse/internal/config"
)

func TestParseMatchesLoad(t *testing.T) {
	path := writeYAML(t, baseValidYAML)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	fromFile, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	fromBytes, err := config.Parse(raw, path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !reflect.DeepEqual(fromBytes, fromFile) {
		t.Fatalf("Parse config differs from Load\nParse: %#v\nLoad:  %#v", fromBytes, fromFile)
	}
}

func TestDefaultsAreCopySafe(t *testing.T) {
	first := config.Defaults()
	second := config.Defaults()

	if first.AgentBackend.Type != "local" || first.PollIntervalS != 30 || first.Caps.Global != 8 {
		t.Fatalf("Defaults returned unexpected core values: %#v", first)
	}
	if first.Models.Coder != "anthropic:claude-sonnet-4-6" || first.Models.Reviewer != "anthropic:claude-opus-4-6" {
		t.Fatalf("Defaults returned unexpected models: %#v", first.Models)
	}
	if !first.Shell.Coder.All || !first.Shell.CoderDocs.All || !first.Shell.Reviewer.All {
		t.Fatalf("Defaults returned restrictive shell policy: %#v", first.Shell)
	}

	first.EnvAllowlist[0] = "MUTATED"
	if second.EnvAllowlist[0] == "MUTATED" {
		t.Fatal("Defaults shares EnvAllowlist storage across calls")
	}
}
