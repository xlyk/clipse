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
