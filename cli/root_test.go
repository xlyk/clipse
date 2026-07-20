package cli_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/xlyk/clipse/cli"
)

func TestRootCmd_Version(t *testing.T) {
	cmd := cli.NewRootCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--version"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	got := buf.String()
	if !strings.Contains(got, "clipse") {
		t.Errorf("version output %q does not contain 'clipse'", got)
	}
	if !strings.Contains(got, "0.1.0-dev") {
		t.Errorf("version output %q does not contain '0.1.0-dev'", got)
	}
}

func TestRootCmd_HelpIncludesConfigure(t *testing.T) {
	cmd := cli.NewRootCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(buf.String(), "configure") {
		t.Fatalf("root help does not list configure:\n%s", buf.String())
	}
}
