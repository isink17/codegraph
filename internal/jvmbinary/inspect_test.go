package jvmbinary

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func validResult(hash string) Result {
	return Result{
		Schema:          ProtocolSchema,
		ProtocolVersion: ProtocolVersion,
		Producer:        Producer{Name: SupportedProducer, Version: SupportedVersion},
		Artifact:        Artifact{SHA256: hash, State: "exact_bytes"},
		Classfile:       Classfile{Owner: "sample/Api", MethodTableState: "parsed"},
		KotlinMetadata:  KotlinMetadata{State: "supported", Version: []int{2, 3, 0}},
		Methods: []Method{{
			KotlinName:  "echo",
			JVMName:     "echo",
			Descriptor:  "(Ljava/lang/String;)Ljava/lang/String;",
			Association: "unique",
			BinaryState: "verified",
			Confidence:  "exact",
		}},
		Trust: TrustStates{
			SourceToBinaryAssociation:  "unknown",
			CurrentGradleTaskSelection: "unknown",
			CurrentBuildFreshness:      "unknown",
			CompilerInputCompleteness:  "unknown",
			CompilerOutputOwnership:    "unknown",
		},
	}
}

func TestDecodeResultRejectsUnsafeOrContradictoryEvidence(t *testing.T) {
	base, err := json.Marshal(validResult(strings.Repeat("a", 64)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeResult(base); err != nil {
		t.Fatalf("valid result rejected: %v", err)
	}

	tests := []struct {
		name string
		edit func(Result) Result
		raw  string
	}{
		{name: "wrong protocol", edit: func(r Result) Result { r.ProtocolVersion++; return r }},
		{name: "unknown producer", edit: func(r Result) Result { r.Producer.Version = "9.0.0"; return r }},
		{name: "promoted freshness", edit: func(r Result) Result { r.Trust.CurrentBuildFreshness = "fresh"; return r }},
		{name: "unsupported metadata", edit: func(r Result) Result { r.KotlinMetadata.Version = []int{2, 4, 0}; return r }},
		{name: "duplicate method", edit: func(r Result) Result { r.Methods = append(r.Methods, r.Methods[0]); return r }},
		{name: "unverified method", edit: func(r Result) Result { r.Methods[0].Association = "ambiguous"; return r }},
		{name: "malformed descriptor", edit: func(r Result) Result { r.Methods[0].Descriptor = "(V)V"; return r }},
		{name: "bad owner", edit: func(r Result) Result { r.Classfile.Owner = "sample.Api"; return r }},
		{name: "method order", edit: func(r Result) Result {
			r.Methods = append(r.Methods, Method{KotlinName: "first", JVMName: "aaa", Descriptor: "()V", Association: "unique", BinaryState: "verified", Confidence: "exact"})
			return r
		}},
		{name: "unknown field", raw: strings.TrimSuffix(string(base), "}") + ",\"abi_trust_complete\":true}"},
		{name: "duplicate key", raw: strings.TrimSuffix(string(base), "}") + ",\"schema\":\"other\"}"},
		{name: "trailing value", raw: string(base) + " {}"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw := test.raw
			if raw == "" {
				result, err := json.Marshal(test.edit(validResult(strings.Repeat("a", 64))))
				if err != nil {
					t.Fatal(err)
				}
				raw = string(result)
			}
			if _, err := decodeResult([]byte(raw)); err == nil {
				t.Fatalf("invalid result was accepted: %s", raw)
			}
		})
	}
}

func TestDescriptorValidation(t *testing.T) {
	for descriptor, want := range map[string]bool{
		"(Ljava/lang/String;)Ljava/lang/String;": true,
		"(I[Ljava/lang/Object;)V":                true,
		"()V":                                    true,
		"(V)V":                                   false,
		"([V)V":                                  false,
		"(Lbad.name;)V":                          false,
		"(Ljava/lang/String;)":                   false,
		"not-a-descriptor":                       false,
	} {
		if got := validMethodDescriptor(descriptor); got != want {
			t.Errorf("validMethodDescriptor(%q) = %v, want %v", descriptor, got, want)
		}
	}
}

func TestStagedClassBytesDoNotFollowLaterPathReplacement(t *testing.T) {
	inputPath := filepath.Join(t.TempDir(), "Api.class")
	original := []byte("original class bytes")
	replacement := []byte("replacement bytes")
	if err := os.WriteFile(inputPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	staged, hash, err := readInput(inputPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inputPath, replacement, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(original)
	if hash != hex.EncodeToString(sum[:]) || string(staged) != string(original) {
		t.Fatalf("staged bytes/hash changed: %q %s", staged, hash)
	}
}

func TestInspectStagesInputBeforeHelperAndVerifiesReturnedHash(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake helper uses a POSIX shell")
	}
	dir := t.TempDir()
	input := filepath.Join(dir, "input.class")
	marker := filepath.Join(dir, "staged-bytes")
	original := []byte("original input bytes")
	if err := os.WriteFile(input, original, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(original)
	response, err := json.Marshal(validResult(hex.EncodeToString(sum[:])))
	if err != nil {
		t.Fatal(err)
	}
	distribution := fakeDistribution(t)
	java := fakeJava(t, "input=\nwhile [ \"$#\" -gt 0 ]; do\n  if [ \"$1\" = \"--input\" ]; then shift; input=\"$1\"; break; fi\n  shift\ndone\nIFS= read -r staged < \"$input\" || true\nprintf '%s' \"$staged\" > "+shellQuote(marker)+"\nprintf 'replacement bytes' > "+shellQuote(input)+"\nprintf '%s\\n' "+shellQuote(string(response))+"\n")
	result, err := Inspect(context.Background(), Options{InputPath: input, InspectorDir: distribution, JavaPath: java, TrustInspector: true})
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if result.Artifact.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("returned hash = %s", result.Artifact.SHA256)
	}
	staged, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if string(staged) != string(original) {
		t.Fatalf("helper observed %q, want staged original bytes %q", staged, original)
	}
	changed, err := os.ReadFile(input)
	if err != nil {
		t.Fatal(err)
	}
	if string(changed) != "replacement bytes" {
		t.Fatalf("test helper did not replace original path: %q", changed)
	}
}

func TestInspectRejectsIncompatibleAndMissingHelpers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake helper uses a POSIX shell")
	}
	dir := t.TempDir()
	input := filepath.Join(dir, "input.class")
	content := []byte("input")
	if err := os.WriteFile(input, content, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	incompatible := validResult(hex.EncodeToString(sum[:]))
	incompatible.ProtocolVersion++
	response, err := json.Marshal(incompatible)
	if err != nil {
		t.Fatal(err)
	}
	distribution := fakeDistribution(t)
	java := fakeJava(t, "printf '%s\\n' "+shellQuote(string(response))+"\n")
	_, err = Inspect(context.Background(), Options{InputPath: input, InspectorDir: distribution, JavaPath: java, TrustInspector: true})
	if err == nil || !strings.Contains(err.Error(), "protocol") {
		t.Fatalf("incompatible helper error = %v", err)
	}

	_, err = Inspect(context.Background(), Options{InputPath: input, InspectorDir: filepath.Join(dir, "missing"), JavaPath: java, TrustInspector: true})
	if err == nil || !strings.Contains(err.Error(), "helper distribution") {
		t.Fatalf("missing helper error = %v", err)
	}
}

func TestInspectBoundsHelperOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake helper uses a POSIX shell")
	}
	dir := t.TempDir()
	input := filepath.Join(dir, "input.class")
	if err := os.WriteFile(input, []byte("input"), 0o600); err != nil {
		t.Fatal(err)
	}
	java := fakeJava(t, "while :; do printf 'xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx'; done\n")
	_, err := Inspect(context.Background(), Options{InputPath: input, InspectorDir: fakeDistribution(t), JavaPath: java, TrustInspector: true})
	if !errors.Is(err, ErrOutputTooLarge) {
		t.Fatalf("oversized output error = %v", err)
	}
}

func TestInspectPropagatesCancellationToHelper(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake helper uses a POSIX shell")
	}
	dir := t.TempDir()
	input := filepath.Join(dir, "input.class")
	if err := os.WriteFile(input, []byte("input"), 0o600); err != nil {
		t.Fatal(err)
	}
	java := fakeJava(t, "while :; do :; done\n")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	_, err := Inspect(ctx, Options{InputPath: input, InspectorDir: fakeDistribution(t), JavaPath: java, TrustInspector: true})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled inspection error = %v", err)
	}
}

func TestInspectEnforcesProcessTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake helper uses a POSIX shell")
	}
	dir := t.TempDir()
	input := filepath.Join(dir, "input.class")
	if err := os.WriteFile(input, []byte("input"), 0o600); err != nil {
		t.Fatal(err)
	}
	java := fakeJava(t, "while :; do :; done\n")
	_, err := inspectWithTimeout(context.Background(), Options{
		InputPath: input, InspectorDir: fakeDistribution(t), JavaPath: java, TrustInspector: true,
	}, 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timed out inspection error = %v", err)
	}
}

func TestReadInputBoundsAndRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	large := filepath.Join(dir, "large.class")
	if err := os.WriteFile(large, make([]byte, MaxClassBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readInput(large); !errors.Is(err, ErrInputTooLarge) {
		t.Fatalf("oversized input error = %v", err)
	}
	if runtime.GOOS != "windows" {
		link := filepath.Join(dir, "link.class")
		if err := os.Symlink(large, link); err != nil {
			t.Fatal(err)
		}
		if _, _, err := readInput(link); err == nil {
			t.Fatal("symlink input accepted")
		}
	}
}

func TestInspectRequiresExplicitTrustAndRuntimePaths(t *testing.T) {
	input := filepath.Join(t.TempDir(), "input.class")
	if err := os.WriteFile(input, []byte("bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(context.Background(), Options{InputPath: input}); err == nil || !strings.Contains(err.Error(), "--trust-inspector") {
		t.Fatalf("untrusted helper error = %v", err)
	}
	_, err := Inspect(context.Background(), Options{
		InputPath: input, InspectorDir: t.TempDir(), JavaPath: filepath.Join(t.TempDir(), "missing-java"), TrustInspector: true,
	})
	if err == nil || !strings.Contains(err.Error(), "Java executable") {
		t.Fatalf("missing Java error = %v", err)
	}
}

func TestStageInspectorCopiesExactDependencyBytes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("staged classpath script test uses POSIX shell")
	}
	dir := t.TempDir()
	lib := filepath.Join(dir, "lib")
	if err := os.Mkdir(lib, 0o700); err != nil {
		t.Fatal(err)
	}
	jarPath := filepath.Join(lib, MainJarName)
	if err := os.WriteFile(jarPath, []byte("trusted helper bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	stage := t.TempDir()
	paths, err := stageInspector(dir, stage)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 {
		t.Fatalf("staged jars = %v", paths)
	}
	if err := os.WriteFile(jarPath, []byte("replacement helper bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "trusted helper bytes" {
		t.Fatalf("staged helper changed to %q", got)
	}
}

func TestInspectCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	input := filepath.Join(t.TempDir(), "input.class")
	if err := os.WriteFile(input, []byte("bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Inspect(ctx, Options{
		InputPath: input, InspectorDir: t.TempDir(), JavaPath: filepath.Join(t.TempDir(), "missing-java"), TrustInspector: true,
	})
	if err == nil {
		t.Fatal("canceled inspection unexpectedly succeeded")
	}
}

func TestJSONNestingBound(t *testing.T) {
	nested := strings.Repeat("[", MaxJSONDepth+2) + "0" + strings.Repeat("]", MaxJSONDepth+2)
	if err := rejectDuplicateKeys([]byte(nested)); err == nil {
		t.Fatal("excessively nested JSON accepted")
	}
}

func TestInspectorTimeoutConstantIsBounded(t *testing.T) {
	if InspectorTimeout <= 0 || InspectorTimeout > time.Minute {
		t.Fatalf("inspector timeout = %s", InspectorTimeout)
	}
}

func fakeDistribution(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	lib := filepath.Join(root, "lib")
	if err := os.Mkdir(lib, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lib, MainJarName), []byte("trusted helper bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func fakeJava(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "java")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
