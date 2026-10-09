package cli

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	codegraph "github.com/isink17/codegraph"
)

func TestRunLicensesPrintsEmbeddedNoticesOffline(t *testing.T) {
	prev := startupVersionCheck
	startupVersionCheck = func(context.Context, io.Writer) { t.Error("licenses ran the network release check") }
	t.Cleanup(func() { startupVersionCheck = prev })
	// No adjacent LICENSE or THIRD_PARTY_NOTICES.
	t.Chdir(t.TempDir())

	run := func() string {
		var out, errOut bytes.Buffer
		if err := Run(context.Background(), []string{"licenses"}, &out, &errOut); err != nil {
			t.Fatalf("Run(licenses) error = %v", err)
		}
		if errOut.Len() != 0 {
			t.Fatalf("Run(licenses) wrote to stderr: %s", errOut.String())
		}
		return out.String()
	}
	got := run()
	if want := codegraph.License + "\n" + codegraph.ThirdPartyNotices; got != want {
		t.Fatal("licenses output is not LICENSE followed by THIRD_PARTY_NOTICES")
	}
	for _, s := range []string{"THIRD-PARTY SOFTWARE NOTICES", "Tree-sitter Lua grammar", "Unicode, Inc."} {
		if !strings.Contains(got, s) {
			t.Errorf("licenses output lacks %q", s)
		}
	}
	if again := run(); again != got {
		t.Fatal("licenses output is not deterministic")
	}
}

func TestRunLicensesHelp(t *testing.T) {
	prev := startupVersionCheck
	startupVersionCheck = func(context.Context, io.Writer) { t.Error("licenses --help ran the network release check") }
	t.Cleanup(func() { startupVersionCheck = prev })

	var out, errOut bytes.Buffer
	if err := Run(context.Background(), []string{"licenses", "--help"}, &out, &errOut); err != nil {
		t.Fatalf("Run(licenses --help) error = %v", err)
	}
	if got := out.String(); !strings.Contains(got, "Usage:") || strings.Contains(got, "THIRD-PARTY") {
		t.Fatalf("licenses --help output unexpected:\n%s", got)
	}
}
