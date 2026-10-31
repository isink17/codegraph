package jvmbinary

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

const (
	ProtocolSchema         = "codegraph.jvm_binary_signature_evidence/v1"
	ProtocolVersion        = 1
	SupportedProducer      = "codegraph-jvm-binary-inspector"
	SupportedVersion       = "1.0.0"
	SupportedMetadataMajor = 2
	SupportedMetadataMinor = 3
	SupportedMetadataPatch = 0
	MaxClassBytes          = 16 << 20
	MaxInspectorBytes      = 64 << 20
	MaxInspectorJars       = 32
	MaxMethods             = 4096
	MaxOutputBytes         = 2 << 20
	MaxStderrBytes         = 16 << 10
	MaxJSONDepth           = 32
	InspectorTimeout       = 15 * time.Second
	MainJarName            = "codegraph-jvm-binary-inspector-1.0.0.jar"
	MainClass              = "org.codegraph.jvm.binary.MainKt"
)

var (
	ErrInputTooLarge  = errors.New("class file exceeds the 16 MiB limit")
	ErrOutputTooLarge = errors.New("JVM inspector output exceeds the 2 MiB limit")
)

type Options struct {
	InputPath      string
	InspectorDir   string
	JavaPath       string
	TrustInspector bool
}

type Result struct {
	Schema                  string         `json:"schema"`
	ProtocolVersion         int            `json:"protocol_version"`
	Producer                Producer       `json:"producer"`
	Artifact                Artifact       `json:"artifact"`
	Classfile               Classfile      `json:"classfile"`
	KotlinMetadata          KotlinMetadata `json:"kotlin_metadata"`
	Methods                 []Method       `json:"methods"`
	UnsupportedDeclarations int            `json:"unsupported_declarations"`
	UnassociatedMethodCount int            `json:"unassociated_method_count"`
	Trust                   TrustStates    `json:"trust"`
}

type Producer struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type Artifact struct {
	SHA256 string `json:"sha256"`
	State  string `json:"state"`
}

type Classfile struct {
	Owner            string `json:"owner"`
	MethodTableState string `json:"method_table_state"`
}

type KotlinMetadata struct {
	State   string `json:"state"`
	Version []int  `json:"version"`
}

type Method struct {
	KotlinName  string `json:"kotlin_name"`
	JVMName     string `json:"jvm_name"`
	Descriptor  string `json:"descriptor"`
	Association string `json:"metadata_association"`
	BinaryState string `json:"binary_signature_state"`
	Confidence  string `json:"confidence"`
}

type TrustStates struct {
	SourceToBinaryAssociation  string `json:"source_to_binary_association"`
	CurrentGradleTaskSelection string `json:"current_gradle_task_selection"`
	CurrentBuildFreshness      string `json:"current_build_freshness"`
	CompilerInputCompleteness  string `json:"compiler_input_completeness"`
	CompilerOutputOwnership    string `json:"compiler_output_ownership"`
}

// Inspect executes only the explicitly authorized helper distribution. The
// class and every helper JAR are copied into a private staging directory before
// the Java process starts, so the bytes validated by Go are the bytes inspected.
func Inspect(ctx context.Context, opts Options) (Result, error) {
	return inspectWithTimeout(ctx, opts, InspectorTimeout)
}

func inspectWithTimeout(ctx context.Context, opts Options, timeout time.Duration) (Result, error) {
	if !opts.TrustInspector {
		return Result{}, errors.New("inspector execution requires --trust-inspector")
	}
	if opts.InputPath == "" || opts.InspectorDir == "" || opts.JavaPath == "" {
		return Result{}, errors.New("input, inspector directory, and Java executable are required")
	}
	javaPath, err := explicitExecutable(opts.JavaPath)
	if err != nil {
		return Result{}, fmt.Errorf("validate Java executable: %w", err)
	}
	stageDir, err := os.MkdirTemp("", "codegraph-jvm-inspector-")
	if err != nil {
		return Result{}, fmt.Errorf("create private inspector staging directory: %w", err)
	}
	defer os.RemoveAll(stageDir)
	if err := os.Chmod(stageDir, 0o700); err != nil && runtime.GOOS != "windows" {
		return Result{}, fmt.Errorf("protect inspector staging directory: %w", err)
	}

	inputBytes, inputHash, err := readInput(opts.InputPath)
	if err != nil {
		return Result{}, err
	}
	inputStage := filepath.Join(stageDir, "input.class")
	if err := writePrivateFile(inputStage, inputBytes); err != nil {
		return Result{}, fmt.Errorf("stage class file: %w", err)
	}
	classPath, err := stageInspector(opts.InspectorDir, stageDir)
	if err != nil {
		return Result{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stdout := &boundedBuffer{limit: MaxOutputBytes, cancel: cancel, overflow: ErrOutputTooLarge}
	stderr := &boundedBuffer{limit: MaxStderrBytes, discardAfterLimit: true}
	javaArgs := []string{
		"-Xms16m", "-Xmx128m", "-Xss1m",
		"-Djava.io.tmpdir=" + stageDir,
		"-cp", strings.Join(classPath, string(os.PathListSeparator)), MainClass,
		"--input", inputStage,
	}
	cmd := exec.CommandContext(ctx, javaPath, javaArgs...)
	cmd.Dir = stageDir
	cmd.Env = inspectorEnvironment(stageDir)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err = cmd.Run()
	if stdout.err != nil {
		return Result{}, stdout.err
	}
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return Result{}, fmt.Errorf("JVM inspector timed out after %s", timeout)
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return Result{}, ctx.Err()
		}
		message := strings.TrimSpace(stderr.String())
		if message != "" {
			return Result{}, fmt.Errorf("JVM inspector failed: %s", boundedDiagnostic(message, 1024))
		}
		return Result{}, fmt.Errorf("JVM inspector failed: %w", err)
	}
	result, err := decodeResult(stdout.Bytes())
	if err != nil {
		return Result{}, fmt.Errorf("validate JVM inspector result: %w", err)
	}
	if result.Artifact.SHA256 != inputHash {
		return Result{}, errors.New("JVM inspector returned a different class-file SHA-256")
	}
	return result, nil
}

func explicitExecutable(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("Java executable path must be absolute")
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("Java executable is not a regular file")
	}
	return path, nil
}

func readInput(path string) ([]byte, string, error) {
	if !filepath.IsAbs(path) {
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, "", fmt.Errorf("resolve class-file path: %w", err)
		}
		path = abs
	}
	linkInfo, err := os.Lstat(path)
	if err != nil {
		return nil, "", fmt.Errorf("inspect class file: %w", err)
	}
	if linkInfo.Mode()&os.ModeSymlink != 0 || !linkInfo.Mode().IsRegular() {
		return nil, "", errors.New("class-file input must be a regular non-symlink file")
	}
	if linkInfo.Size() > MaxClassBytes {
		return nil, "", ErrInputTooLarge
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, "", fmt.Errorf("open class file: %w", err)
	}
	defer f.Close()
	openedInfo, err := f.Stat()
	if err != nil {
		return nil, "", fmt.Errorf("stat opened class file: %w", err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(linkInfo, openedInfo) {
		return nil, "", errors.New("class-file input changed while opening")
	}
	bytes, err := io.ReadAll(io.LimitReader(f, MaxClassBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("read class file: %w", err)
	}
	if len(bytes) > MaxClassBytes {
		return nil, "", ErrInputTooLarge
	}
	hash := sha256.Sum256(bytes)
	return bytes, hex.EncodeToString(hash[:]), nil
}

func stageInspector(root, stageDir string) ([]string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve inspector directory: %w", err)
	}
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return nil, fmt.Errorf("inspect helper distribution: %w", err)
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return nil, errors.New("inspector distribution must be a directory, not a symlink")
	}
	libDir := filepath.Join(root, "lib")
	libInfo, err := os.Lstat(libDir)
	if err != nil {
		return nil, fmt.Errorf("inspect helper runtime libraries: %w", err)
	}
	if libInfo.Mode()&os.ModeSymlink != 0 || !libInfo.IsDir() {
		return nil, errors.New("inspector distribution has no regular lib directory")
	}
	entries, err := os.ReadDir(libDir)
	if err != nil {
		return nil, fmt.Errorf("list helper runtime libraries: %w", err)
	}
	if len(entries) == 0 || len(entries) > MaxInspectorJars {
		return nil, fmt.Errorf("inspector must contain between 1 and %d runtime JARs", MaxInspectorJars)
	}
	mainJar := false
	var paths []string
	total := int64(0)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jar") {
			return nil, fmt.Errorf("unexpected file in inspector lib directory: %q", entry.Name())
		}
		path := filepath.Join(libDir, entry.Name())
		linkInfo, err := os.Lstat(path)
		if err != nil {
			return nil, fmt.Errorf("inspect helper JAR: %w", err)
		}
		if linkInfo.Mode()&os.ModeSymlink != 0 || !linkInfo.Mode().IsRegular() {
			return nil, fmt.Errorf("helper runtime dependency %q is not a regular file", entry.Name())
		}
		total += linkInfo.Size()
		if linkInfo.Size() < 0 || total > MaxInspectorBytes {
			return nil, errors.New("helper runtime dependencies exceed the 64 MiB limit")
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("open helper JAR %q: %w", entry.Name(), err)
		}
		openedInfo, err := f.Stat()
		if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(linkInfo, openedInfo) {
			_ = f.Close()
			return nil, fmt.Errorf("helper JAR %q changed while opening", entry.Name())
		}
		dest := filepath.Join(stageDir, entry.Name())
		out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("stage helper JAR %q: %w", entry.Name(), err)
		}
		_, copyErr := io.Copy(out, io.LimitReader(f, linkInfo.Size()+1))
		closeInErr := f.Close()
		closeOutErr := out.Close()
		if copyErr != nil || closeInErr != nil || closeOutErr != nil {
			return nil, errors.Join(copyErr, closeInErr, closeOutErr)
		}
		stagedInfo, err := os.Stat(dest)
		if err != nil || stagedInfo.Size() != linkInfo.Size() {
			return nil, fmt.Errorf("helper JAR %q changed while staging", entry.Name())
		}
		if entry.Name() == MainJarName {
			mainJar = true
		}
		paths = append(paths, dest)
	}
	if !mainJar {
		return nil, fmt.Errorf("inspector distribution is missing %s", MainJarName)
	}
	// Gradle installDist uses a stable main JAR name; keep it first and the
	// remaining dependency classpath deterministic by filename.
	sort.Slice(paths, func(i, j int) bool {
		if filepath.Base(paths[i]) == MainJarName {
			return true
		}
		if filepath.Base(paths[j]) == MainJarName {
			return false
		}
		return filepath.Base(paths[i]) < filepath.Base(paths[j])
	})
	return paths, nil
}

func writePrivateFile(path string, content []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(content)
	syncErr := f.Sync()
	closeErr := f.Close()
	return errors.Join(writeErr, syncErr, closeErr)
}

func inspectorEnvironment(tempDir string) []string {
	if runtime.GOOS != "windows" {
		return []string{"TMPDIR=" + tempDir}
	}
	var env []string
	for _, key := range []string{"SYSTEMROOT", "WINDIR", "TEMP", "TMP"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}

type boundedBuffer struct {
	bytes             bytes.Buffer
	limit             int
	cancel            context.CancelFunc
	overflow          error
	discardAfterLimit bool
	err               error
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	remaining := b.limit - b.bytes.Len()
	if remaining <= 0 {
		if b.discardAfterLimit {
			return len(p), nil
		}
		b.err = b.overflow
		if b.cancel != nil {
			b.cancel()
		}
		return 0, b.err
	}
	if len(p) > remaining {
		_, _ = b.bytes.Write(p[:remaining])
		if b.discardAfterLimit {
			return len(p), nil
		}
		b.err = b.overflow
		if b.cancel != nil {
			b.cancel()
		}
		return remaining, b.err
	}
	return b.bytes.Write(p)
}

func (b *boundedBuffer) Bytes() []byte  { return b.bytes.Bytes() }
func (b *boundedBuffer) String() string { return b.bytes.String() }

func boundedDiagnostic(s string, limit int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\t' {
			return ' '
		}
		return r
	}, s)
	if len(s) > limit {
		return s[:limit] + "…"
	}
	return s
}

func decodeResult(data []byte) (Result, error) {
	if len(data) == 0 || len(data) > MaxOutputBytes {
		return Result{}, errors.New("empty or oversized JSON response")
	}
	if err := rejectDuplicateKeys(data); err != nil {
		return Result{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var result Result
	if err := decoder.Decode(&result); err != nil {
		return Result{}, err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return Result{}, err
	}
	if err := validateResult(result); err != nil {
		return Result{}, err
	}
	return result, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func rejectDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var readValue func(depth int) error
	readValue = func(depth int) error {
		if depth > MaxJSONDepth {
			return errors.New("JSON nesting exceeds the limit")
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			keys := map[string]struct{}{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("JSON object key is not a string")
				}
				if _, found := keys[key]; found {
					return fmt.Errorf("duplicate JSON key %q", key)
				}
				keys[key] = struct{}{}
				if err := readValue(depth + 1); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim('}') {
				return errors.New("malformed JSON object")
			}
		case '[':
			for decoder.More() {
				if err := readValue(depth + 1); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim(']') {
				return errors.New("malformed JSON array")
			}
		default:
			return errors.New("unexpected JSON delimiter")
		}
		return nil
	}
	if err := readValue(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func validateResult(r Result) error {
	if r.Schema != ProtocolSchema || r.ProtocolVersion != ProtocolVersion {
		return errors.New("unsupported inspector protocol")
	}
	if r.Producer.Name != SupportedProducer || r.Producer.Version != SupportedVersion {
		return errors.New("unsupported inspector producer version")
	}
	if len(r.Artifact.SHA256) != 64 || strings.ToLower(r.Artifact.SHA256) != r.Artifact.SHA256 {
		return errors.New("invalid class-file SHA-256")
	}
	if _, err := hex.DecodeString(r.Artifact.SHA256); err != nil || r.Artifact.State != "exact_bytes" {
		return errors.New("invalid binary artifact identity")
	}
	if !validInternalName(r.Classfile.Owner) || r.Classfile.MethodTableState != "parsed" {
		return errors.New("invalid classfile identity")
	}
	switch r.KotlinMetadata.State {
	case "supported", "unsupported_kind", "unsupported_class_shape":
		if !equalInts(r.KotlinMetadata.Version, []int{SupportedMetadataMajor, SupportedMetadataMinor, SupportedMetadataPatch}) {
			return errors.New("unsupported Kotlin metadata version")
		}
		if r.KotlinMetadata.State != "supported" && len(r.Methods) != 0 {
			return errors.New("unsupported Kotlin class shape contains verified methods")
		}
	case "absent":
		if len(r.KotlinMetadata.Version) != 0 {
			return errors.New("metadata-absent response has a version")
		}
		if len(r.Methods) != 0 {
			return errors.New("methods cannot be associated without Kotlin metadata")
		}
	default:
		return errors.New("invalid Kotlin metadata state")
	}
	if r.UnsupportedDeclarations < 0 || r.UnassociatedMethodCount < 0 || r.UnassociatedMethodCount > 65_535 || len(r.Methods) > MaxMethods {
		return errors.New("inspector declaration count exceeds the limit")
	}
	seen := make(map[string]struct{}, len(r.Methods))
	last := ""
	for i, method := range r.Methods {
		if method.KotlinName == "" || len(method.KotlinName) > 65535 || !validMethodName(method.JVMName) || !validMethodDescriptor(method.Descriptor) {
			return fmt.Errorf("method %d has invalid signature", i)
		}
		if method.Association != "unique" || method.BinaryState != "verified" || method.Confidence != "exact" {
			return fmt.Errorf("method %d is not uniquely verified", i)
		}
		key := method.JVMName + "\x00" + method.Descriptor
		if _, exists := seen[key]; exists {
			return errors.New("duplicate JVM method signature")
		}
		seen[key] = struct{}{}
		if i > 0 && key < last {
			return errors.New("method records are not in deterministic order")
		}
		last = key
	}
	if r.Trust.SourceToBinaryAssociation != "unknown" || r.Trust.CurrentGradleTaskSelection != "unknown" || r.Trust.CurrentBuildFreshness != "unknown" || r.Trust.CompilerInputCompleteness != "unknown" || r.Trust.CompilerOutputOwnership != "unknown" {
		return errors.New("inspector attempted to promote a source-build trust dimension")
	}
	if r.KotlinMetadata.State != "supported" && len(r.Methods) != 0 {
		return errors.New("metadata association is not supported")
	}
	return nil
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func validInternalName(name string) bool {
	if name == "" || len(name) > 65535 || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") || strings.ContainsAny(name, ".;[\\") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" {
			return false
		}
	}
	return true
}

func validMethodName(name string) bool {
	if name == "" || len(name) > 65535 || strings.ContainsAny(name, ".;[/<>") {
		return false
	}
	return true
}

func validMethodDescriptor(d string) bool {
	if d == "" || len(d) > 65535 || d[0] != '(' {
		return false
	}
	i := 1
	for i < len(d) && d[i] != ')' {
		if !validDescriptorType(d, &i, false) {
			return false
		}
	}
	if i >= len(d) || d[i] != ')' {
		return false
	}
	i++
	if !validDescriptorType(d, &i, true) {
		return false
	}
	return i == len(d)
}

func validDescriptorType(d string, i *int, allowVoid bool) bool {
	if *i >= len(d) {
		return false
	}
	arrays := 0
	for *i < len(d) && d[*i] == '[' {
		arrays++
		*i++
		if arrays > 255 {
			return false
		}
	}
	if *i >= len(d) {
		return false
	}
	c := d[*i]
	*i++
	switch c {
	case 'B', 'C', 'D', 'F', 'I', 'J', 'S', 'Z':
		return true
	case 'V':
		return allowVoid && arrays == 0
	case 'L':
		start := *i
		for *i < len(d) && d[*i] != ';' {
			*i++
		}
		if *i >= len(d) || *i == start || !validInternalName(d[start:*i]) {
			return false
		}
		*i++
		return true
	default:
		return false
	}
}
