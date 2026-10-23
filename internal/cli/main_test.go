package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/config"
)

// testHome is the private CODEGRAPH_HOME every test in this package starts
// with, so no test reads or writes the real user configuration. Tests that
// need their own home still override it with t.Setenv.
var testHome string

// testHomeEnv marks helper processes that tests re-execute from this binary:
// they keep the environment, and the CODEGRAPH_HOME, their parent test chose.
const testHomeEnv = "CODEGRAPH_CLI_TEST_HOME"

func TestMain(m *testing.M) {
	if testHome = os.Getenv(testHomeEnv); testHome != "" {
		os.Exit(m.Run())
	}
	dir, err := os.MkdirTemp("", "codegraph-cli-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "cli tests: private CODEGRAPH_HOME:", err)
		os.Exit(1)
	}
	testHome = filepath.Join(dir, "home")
	for k, v := range map[string]string{"CODEGRAPH_HOME": testHome, testHomeEnv: testHome} {
		if err := os.Setenv(k, v); err != nil {
			fmt.Fprintln(os.Stderr, "cli tests: set", k+":", err)
			os.Exit(1)
		}
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func TestHostConfigCannotReachCLITests(t *testing.T) {
	if got := os.Getenv("CODEGRAPH_HOME"); testHome == "" || got != testHome {
		t.Fatalf("CODEGRAPH_HOME = %q, want the private test home %q", got, testHome)
	}
	prev := startupVersionCheck
	startupVersionCheck = func(context.Context, io.Writer) {}
	t.Cleanup(func() { startupVersionCheck = prev })

	host := t.TempDir()
	for _, env := range []string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME", "AppData", "LocalAppData"} {
		t.Setenv(env, host)
	}
	t.Setenv("CODEGRAPH_HOME", "")
	hostConfig, err := config.ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hostConfig, host) {
		t.Fatalf("default config path %q is not under the fake host home %q", hostConfig, host)
	}
	if err := os.MkdirAll(filepath.Dir(hostConfig), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hostConfig, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(); err == nil {
		t.Fatal("control: the poisoned host config was not read without CODEGRAPH_HOME")
	}

	t.Setenv("CODEGRAPH_HOME", testHome)
	if got, err := config.ConfigPath(); err != nil || !strings.HasPrefix(got, testHome) {
		t.Fatalf("config path = %q, %v; want it under the private home %q", got, err, testHome)
	}
	var out, errOut bytes.Buffer
	if err := Run(context.Background(), []string{"help", "find_symbol"}, &out, &errOut); err != nil {
		t.Fatalf("Run(help find_symbol) read the host config: %v", err)
	}
}
