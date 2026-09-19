package platform

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// nativeBoundaryCanaries are legal-everywhere repository files whose logical
// identity must survive the native<->logical boundary on every host. The
// native spelling is assembled with filepath.Join so Windows creates real
// backslash-separated paths; the logical expectation is written out literally
// so the proof never re-derives its own answer from the code under test.
var nativeBoundaryCanaries = []struct {
	nativeRel string
	logical   string
}{
	{filepath.Join("src", "plain.go"), "src/plain.go"},
	{filepath.Join("dir with space", "file with space.go"), "dir with space/file with space.go"},
	{filepath.Join("unicode-ž", "λ_file.go"), "unicode-ž/λ_file.go"},
	{filepath.Join("nested", "x", "y.go"), "nested/x/y.go"},
}

// TestNativeLogicalBoundaryOnRealFilesystem is the runtime counterpart to the
// synthetic table tests in this package: it creates real files under a real
// temporary root and drives both directions of the platform path boundary
// against actual filesystem resolution. On Windows the native spellings
// genuinely contain '\', which is exactly the condition the logical '/'
// repository identity has to survive.
func TestNativeLogicalBoundaryOnRealFilesystem(t *testing.T) {
	root := t.TempDir()
	for _, c := range nativeBoundaryCanaries {
		full := filepath.Join(root, c.nativeRel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("host cannot create directory for %q: %v", c.nativeRel, err)
		}
		if err := os.WriteFile(full, []byte("package p\n"), 0o644); err != nil {
			t.Fatalf("host cannot create %q: %v", c.nativeRel, err)
		}
	}

	// The fixture is only meaningful if the host really uses '\' natively.
	if runtime.GOOS == "windows" {
		nested := nativeBoundaryCanaries[3].nativeRel
		if !strings.Contains(nested, `\`) {
			t.Fatalf("windows native spelling %q has no native separator", nested)
		}
	}

	t.Run("native_to_logical", func(t *testing.T) {
		// Walk the real tree: the relative spellings come from the filesystem,
		// not from the fixture strings, so a host that rewrote a name fails here.
		want := map[string]struct{}{}
		for _, c := range nativeBoundaryCanaries {
			want[c.logical] = struct{}{}
		}
		got := map[string]struct{}{}
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			logical, err := NativeRelativeToLogical(rel)
			if err != nil {
				t.Fatalf("NativeRelativeToLogical(%q): %v", rel, err)
			}
			if strings.ContainsRune(logical, filepath.Separator) && filepath.Separator != '/' {
				t.Fatalf("logical identity %q retains native separator", logical)
			}
			if filepath.IsAbs(logical) || filepath.VolumeName(logical) != "" || strings.HasPrefix(logical, "/") {
				t.Fatalf("logical identity %q is not repository-relative", logical)
			}
			got[logical] = struct{}{}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) {
			t.Fatalf("logical set = %v, want %v", got, want)
		}
		for k := range want {
			if _, ok := got[k]; !ok {
				t.Fatalf("logical set = %v, missing %q", got, k)
			}
		}
	})

	t.Run("logical_to_native", func(t *testing.T) {
		// The inverse boundary is proved by filesystem resolution, not by the
		// textual spelling the helper happens to emit.
		for _, c := range nativeBoundaryCanaries {
			native, err := NativePath(root, c.logical)
			if err != nil {
				t.Fatalf("NativePath(%q): %v", c.logical, err)
			}
			info, err := os.Stat(native)
			if err != nil {
				t.Fatalf("NativePath(%q) = %q does not resolve: %v", c.logical, native, err)
			}
			if info.IsDir() {
				t.Fatalf("NativePath(%q) = %q resolved a directory", c.logical, native)
			}
			wantNative := filepath.Join(root, c.nativeRel)
			if native != wantNative {
				t.Fatalf("NativePath(%q) = %q, want the file created at %q", c.logical, native, wantNative)
			}
			same, err := os.Stat(wantNative)
			if err != nil || !os.SameFile(info, same) {
				t.Fatalf("NativePath(%q) targeted a different file than %q (%v)", c.logical, wantNative, err)
			}
		}
	})

	t.Run("public_input_is_logical", func(t *testing.T) {
		// The public repository identity is the '/' spelling on every host,
		// including Windows: it must be accepted verbatim and unchanged.
		for _, c := range nativeBoundaryCanaries {
			got, err := PublicRepositoryPath(c.logical)
			if err != nil {
				t.Fatalf("PublicRepositoryPath(%q): %v", c.logical, err)
			}
			if got != c.logical {
				t.Fatalf("PublicRepositoryPath(%q) = %q", c.logical, got)
			}
		}
	})

	t.Run("no_absolute_leak", func(t *testing.T) {
		// A native absolute path is never a repository identity, whatever the
		// host's absolute spelling looks like (drive letter, UNC or '/').
		absolutes := []string{root, filepath.Join(root, nativeBoundaryCanaries[0].nativeRel)}
		for _, abs := range absolutes {
			if _, err := NativeRelativeToLogical(abs); err == nil {
				t.Fatalf("NativeRelativeToLogical accepted absolute %q", abs)
			}
			if _, err := LogicalRepositoryPath(filepath.ToSlash(abs)); err == nil {
				t.Fatalf("LogicalRepositoryPath accepted absolute %q", abs)
			}
		}
	})
}
