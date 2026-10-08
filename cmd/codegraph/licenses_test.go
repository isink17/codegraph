package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	codegraph "github.com/isink17/codegraph"
)

// The standalone binary must print its notices with no files next to it and
// no network: build it, then run it from an empty directory.
func TestBinaryPrintsEmbeddedLicensesFromEmptyDir(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "codegraph")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	run := exec.Command(bin, "licenses")
	run.Dir = dir
	run.Env = append(os.Environ(), "CODEGRAPH_HOME="+filepath.Join(dir, "home"))
	out, err := run.Output()
	if err != nil {
		t.Fatalf("codegraph licenses: %v", err)
	}
	if string(out) != codegraph.License+"\n"+codegraph.ThirdPartyNotices {
		t.Fatal("binary output is not the embedded LICENSE and THIRD_PARTY_NOTICES")
	}
}
