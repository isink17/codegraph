package buildevidence

import (
	"archive/zip"
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/textproto"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	CompilerProbeSchema    = "codegraph.kotlin_compiler_probe/v2"
	CompilerProbeProtocol  = 2
	CompilerProbeStatus    = "CORRELATED_COMPILER_CLASS_OBSERVATION"
	MaxCompilerProbeRecord = 64 << 10
	maxCompilerProbeJar    = 64 << 20
)

var (
	probeTokenPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	probeHashPattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// CompilerProbe is a separate diagnostic artifact. It never upgrades v2 proof
// dimensions or authorizes compiler/ABI trust.
type CompilerProbe struct {
	Schema                     string `json:"schema"`
	Status                     string `json:"status"`
	ReasonCode                 string `json:"reason_code,omitempty"`
	ProtocolVersion            int    `json:"protocol_version"`
	InvocationToken            string `json:"invocation_token"`
	ProbeVersion               string `json:"probe_version"`
	ProbeJARSHA256             string `json:"probe_jar_sha256"`
	CompilerClass              string `json:"compiler_class,omitempty"`
	CompilerClassloader        string `json:"compiler_classloader,omitempty"`
	CompilerCodeSourceName     string `json:"compiler_codesource_name,omitempty"`
	CompilerCodeSourceSHA256   string `json:"compiler_codesource_sha256,omitempty"`
	CompilerClassSHA256        string `json:"compiler_class_resource_sha256,omitempty"`
	Project                    string `json:"project"`
	Compilation                string `json:"compilation"`
	Task                       string `json:"task"`
	CallbackExecuted           bool   `json:"callback_executed"`
	ProcessID                  int64  `json:"process_id,omitempty"`
	ProcessStart               string `json:"process_start,omitempty"`
	RuntimeVersion             string `json:"runtime_version,omitempty"`
	RuntimeVendor              string `json:"runtime_vendor,omitempty"`
	SelectedToolchainVersion   string `json:"selected_toolchain_version,omitempty"`
	RequestedExecutionStrategy string `json:"requested_execution_strategy"`
	GradleProcessID            int64  `json:"gradle_process_id,omitempty"`
	ProcessRelationship        string `json:"process_relationship"`
	EvidenceFingerprint        string `json:"evidence_fingerprint"`
}

type compilerProbeRecord struct {
	Schema                     string `json:"schema"`
	ProtocolVersion            int    `json:"protocol_version"`
	Status                     string `json:"status"`
	InvocationToken            string `json:"invocation_token"`
	ProbeVersion               string `json:"probe_version"`
	ProbeJARSHA256             string `json:"probe_jar_sha256"`
	CompilerClass              string `json:"compiler_class"`
	CompilerClassloader        string `json:"compiler_classloader"`
	CompilerCodeSourceName     string `json:"compiler_codesource_name"`
	CompilerCodeSourceSHA256   string `json:"compiler_codesource_sha256"`
	CompilerClassSHA256        string `json:"compiler_class_resource_sha256"`
	Project                    string `json:"project"`
	Compilation                string `json:"compilation"`
	Task                       string `json:"task"`
	CallbackExecuted           bool   `json:"callback_executed"`
	ProcessID                  int64  `json:"process_id"`
	ProcessStart               string `json:"process_start"`
	RuntimeVersion             string `json:"runtime_version"`
	RuntimeVendor              string `json:"runtime_vendor"`
	RequestedExecutionStrategy string `json:"requested_execution_strategy"`
}

type probeInvocation struct {
	token       string
	jarPath     string
	jarSHA256   string
	recordPath  string
	project     string
	compilation string
	strategy    string
}

func prepareCompilerProbe(root, jarPath, outputPath, project, compilation, strategy string) (*probeInvocation, error) {
	jarAbs, err := filepath.Abs(jarPath)
	if err != nil {
		return nil, fmt.Errorf("resolve compiler probe JAR: %w", err)
	}
	jarAbs, err = filepath.EvalSymlinks(jarAbs)
	if err != nil {
		return nil, fmt.Errorf("resolve compiler probe JAR symlink: %w", err)
	}
	if _, inside := relativeWithin(root, jarAbs); inside {
		return nil, errors.New("compiler probe JAR must be outside the target repository")
	}
	if strings.ContainsAny(jarAbs, ",\r\n\x00") {
		return nil, errors.New("compiler probe JAR path contains unsupported compiler-option characters")
	}
	info, err := os.Stat(jarAbs)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxCompilerProbeJar {
		return nil, errors.New("compiler probe JAR must be a nonempty regular file no larger than 64 MiB")
	}
	if err := validateCompilerProbeManifest(jarAbs, info.Size()); err != nil {
		return nil, err
	}
	file, err := os.Open(jarAbs)
	if err != nil {
		return nil, err
	}
	openedInfo, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if !os.SameFile(info, openedInfo) {
		file.Close()
		return nil, errors.New("compiler probe JAR changed during validation")
	}
	h := sha256.New()
	readBytes, copyErr := io.Copy(h, io.LimitReader(file, maxCompilerProbeJar+1))
	closeErr := file.Close()
	if copyErr != nil {
		return nil, copyErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if readBytes != info.Size() {
		return nil, errors.New("compiler probe JAR changed while being hashed")
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, fmt.Errorf("generate compiler probe invocation token: %w", err)
	}
	probeOutput := outputPath + ".compiler-probe.json"
	if _, err := os.Lstat(probeOutput); err == nil {
		return nil, errors.New("compiler probe output already exists")
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if strategy == "" {
		strategy = "default"
	}
	return &probeInvocation{token: hex.EncodeToString(tokenBytes), jarPath: jarAbs, jarSHA256: hex.EncodeToString(h.Sum(nil)), project: project, compilation: compilation, strategy: strategy}, nil
}

func validateCompilerProbeManifest(path string, size int64) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	archive, err := zip.NewReader(file, size)
	if err != nil {
		return fmt.Errorf("compiler probe is not a valid JAR: %w", err)
	}
	var manifest *zip.File
	for _, entry := range archive.File {
		if entry.Name != "META-INF/MANIFEST.MF" {
			continue
		}
		if manifest != nil {
			return errors.New("compiler probe JAR has duplicate manifests")
		}
		manifest = entry
	}
	if manifest == nil || manifest.UncompressedSize64 > 64<<10 {
		return errors.New("compiler probe JAR manifest is missing or oversized")
	}
	stream, err := manifest.Open()
	if err != nil {
		return err
	}
	data, readErr := io.ReadAll(io.LimitReader(stream, (64<<10)+1))
	closeErr := stream.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	if len(data) > 64<<10 {
		return errors.New("compiler probe JAR manifest exceeds 64 KiB")
	}
	header, err := textproto.NewReader(bufio.NewReader(bytes.NewReader(data))).ReadMIMEHeader()
	if err != nil {
		return fmt.Errorf("read compiler probe JAR manifest: %w", err)
	}
	for key, want := range map[string]string{
		"Implementation-Version":  "2.0.0",
		"Cg35-Protocol-Version":   "2",
		"Kotlin-Compiler-Version": "2.3.20",
	} {
		values := header.Values(key)
		if len(values) != 1 || strings.TrimSpace(values[0]) != want {
			return fmt.Errorf("compiler probe JAR %s is unsupported or ambiguous", key)
		}
	}
	return nil
}

func readCompilerProbe(invocation *probeInvocation, model gradleModel, artifact Artifact) (CompilerProbe, error) {
	out := CompilerProbe{
		Schema: CompilerProbeSchema, Status: "UNKNOWN", ReasonCode: "CALLBACK_MISSING",
		ProtocolVersion: CompilerProbeProtocol, InvocationToken: invocation.token,
		Project: model.Project, Compilation: model.Compilation, Task: model.CompileTask,
		EvidenceFingerprint: artifact.InputFingerprint, GradleProcessID: model.GradleProcessID,
		RequestedExecutionStrategy: invocation.strategy, ProcessRelationship: "UNKNOWN",
	}
	if invocation.project != model.Project || invocation.compilation != model.Compilation {
		return out, errors.New("compiler probe selected project or compilation differs from exporter request")
	}
	if _, err := os.Lstat(invocation.recordPath + ".duplicate"); err == nil {
		return out, errors.New("compiler probe callback produced duplicate records")
	} else if !os.IsNotExist(err) {
		return out, err
	}
	if model.TaskOutcome == "UP_TO_DATE" || model.TaskOutcome == "FROM_CACHE" || model.TaskOutcome == "NO_SOURCE" || model.TaskSkipped {
		out.ReasonCode = "TASK_SKIPPED_NO_FRESH_CALLBACK"
		if _, err := os.Lstat(invocation.recordPath); err == nil {
			return out, errors.New("compiler probe record exists although selected task was skipped")
		} else if !os.IsNotExist(err) {
			return out, err
		}
		return out, nil
	}
	info, err := os.Lstat(invocation.recordPath)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return out, fmt.Errorf("stat compiler probe record: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > MaxCompilerProbeRecord {
		return out, errors.New("compiler probe record is not a bounded regular file")
	}
	file, err := os.Open(invocation.recordPath)
	if err != nil {
		return out, fmt.Errorf("read compiler probe record: %w", err)
	}
	openedInfo, err := file.Stat()
	if err != nil {
		file.Close()
		return out, err
	}
	if !os.SameFile(info, openedInfo) || !openedInfo.Mode().IsRegular() || openedInfo.Size() > MaxCompilerProbeRecord {
		file.Close()
		return out, errors.New("compiler probe record changed or exceeded its size limit")
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxCompilerProbeRecord+1))
	closeErr := file.Close()
	if err != nil {
		return out, err
	}
	if closeErr != nil {
		return out, closeErr
	}
	if len(data) > MaxCompilerProbeRecord {
		return out, errors.New("compiler probe record exceeds its size limit")
	}
	if err := validateUniqueJSONKeys(data); err != nil {
		return out, fmt.Errorf("invalid compiler probe JSON: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var record compilerProbeRecord
	if err := dec.Decode(&record); err != nil {
		return out, fmt.Errorf("decode compiler probe record: %w", err)
	}
	var trailer any
	if err := dec.Decode(&trailer); err != io.EOF {
		return out, errors.New("compiler probe record contains trailing JSON")
	}
	if record.Schema != CompilerProbeSchema || record.ProtocolVersion != CompilerProbeProtocol {
		return out, errors.New("compiler probe protocol mismatch")
	}
	if !probeTokenPattern.MatchString(record.InvocationToken) || record.InvocationToken != invocation.token {
		return out, errors.New("compiler probe invocation token mismatch")
	}
	if record.Project != model.Project || record.Compilation != model.Compilation || record.Task != model.CompileTask {
		return out, errors.New("compiler probe selected compilation mismatch")
	}
	if record.RequestedExecutionStrategy != invocation.strategy {
		return out, errors.New("compiler probe execution strategy mismatch")
	}
	if record.ProbeVersion != "2.0.0" {
		return out, errors.New("compiler probe implementation identity mismatch")
	}
	if record.ProbeJARSHA256 != "" && (!probeHashPattern.MatchString(record.ProbeJARSHA256) || record.ProbeJARSHA256 != invocation.jarSHA256) {
		return out, errors.New("compiler probe implementation identity mismatch")
	}
	if (record.CompilerCodeSourceSHA256 != "" && !probeHashPattern.MatchString(record.CompilerCodeSourceSHA256)) ||
		(record.CompilerClassSHA256 != "" && !probeHashPattern.MatchString(record.CompilerClassSHA256)) ||
		!probeClassloaderPattern.MatchString(record.CompilerClassloader) || !probeCompilerClassPattern.MatchString(record.CompilerClass) ||
		!probeSourceNamePattern.MatchString(record.CompilerCodeSourceName) ||
		!validProbeRuntime(record.RuntimeVersion) || !validOptionalProbeRuntime(record.RuntimeVendor) ||
		!validProbeText(record.ProcessStart, 64) {
		return out, errors.New("compiler probe contains malformed identity fields")
	}
	if !record.CallbackExecuted || record.Status != "CALLBACK_OBSERVED" {
		return out, errors.New("compiler probe callback record is incomplete")
	}
	out.ProbeVersion, out.ProbeJARSHA256 = record.ProbeVersion, record.ProbeJARSHA256
	out.CompilerClass, out.CompilerClassloader = record.CompilerClass, record.CompilerClassloader
	out.CompilerCodeSourceName = record.CompilerCodeSourceName
	out.CompilerCodeSourceSHA256, out.CompilerClassSHA256 = record.CompilerCodeSourceSHA256, record.CompilerClassSHA256
	out.CallbackExecuted, out.ProcessID = true, record.ProcessID
	out.ProcessStart, out.RuntimeVersion = record.ProcessStart, record.RuntimeVersion
	out.RuntimeVendor = record.RuntimeVendor
	out.SelectedToolchainVersion = model.SelectedToolchainVersion
	out.RequestedExecutionStrategy = record.RequestedExecutionStrategy
	if model.GradleProcessID > 0 {
		out.ProcessRelationship = "SEPARATE_PROCESS"
		if model.GradleProcessID == record.ProcessID {
			out.ProcessRelationship = "SAME_PROCESS"
		}
	} else {
		out.ProcessRelationship = "UNKNOWN"
	}
	if (record.RequestedExecutionStrategy == "daemon" && out.ProcessRelationship != "SEPARATE_PROCESS") ||
		(record.RequestedExecutionStrategy == "in-process" && out.ProcessRelationship != "SAME_PROCESS") {
		out.ReasonCode = "EXECUTION_STRATEGY_PROCESS_RELATIONSHIP_MISMATCH"
		return out, nil
	}
	if record.ProbeJARSHA256 == "" || !validProbeText(record.CompilerClassloader, 256) || record.ProcessID <= 0 ||
		!validProbeText(record.ProcessStart, 64) || !validProbeText(record.RuntimeVersion, 64) ||
		model.KotlinPluginVersion != "2.3.20" || record.CompilerClass != "org.jetbrains.kotlin.cli.jvm.K2JVMCompiler" ||
		!probeHashPattern.MatchString(record.CompilerCodeSourceSHA256) || !probeHashPattern.MatchString(record.CompilerClassSHA256) {
		out.ReasonCode = "COMPILER_IDENTITY_INCOMPLETE_OR_UNEXPECTED"
		return out, nil
	}
	out.Status, out.ReasonCode = CompilerProbeStatus, ""
	return out, nil
}

func validProbeText(s string, limit int) bool {
	return s != "" && len(s) <= limit && !strings.ContainsAny(s, "\r\n\x00")
}

var (
	probeClassloaderPattern   = regexp.MustCompile(`^(?:[A-Za-z0-9.$_]{1,256})?$`)
	probeCompilerClassPattern = regexp.MustCompile(`^(?:[A-Za-z0-9.$_]{1,256})?$`)
	probeSourceNamePattern    = regexp.MustCompile(`^(?:[A-Za-z0-9._+-]{1,128}|unavailable)?$`)
	probeRuntimePattern       = regexp.MustCompile(`^(?:[A-Za-z0-9._+ -]{1,64}|unknown)$`)
)

func validProbeRuntime(value string) bool { return probeRuntimePattern.MatchString(value) }

func validOptionalProbeRuntime(value string) bool { return value == "" || validProbeRuntime(value) }

func (p CompilerProbe) Marshal() ([]byte, error) {
	if p.Schema != CompilerProbeSchema || (p.Status != "UNKNOWN" && p.Status != CompilerProbeStatus) ||
		p.ProtocolVersion != CompilerProbeProtocol || !probeTokenPattern.MatchString(p.InvocationToken) ||
		!probeHashPattern.MatchString(p.EvidenceFingerprint) || p.Project == "" || p.Compilation == "" || p.Task == "" {
		return nil, errors.New("invalid compiler probe diagnostic artifact")
	}
	if p.Status == CompilerProbeStatus && p.ProcessRelationship != "SAME_PROCESS" && p.ProcessRelationship != "SEPARATE_PROCESS" && p.ProcessRelationship != "UNKNOWN" {
		return nil, errors.New("invalid compiler probe process relationship")
	}
	if !validOptionalProbeRuntime(p.RuntimeVendor) || !validOptionalProbeRuntime(p.SelectedToolchainVersion) {
		return nil, errors.New("invalid compiler probe runtime identity text")
	}
	data, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	if len(data) > MaxCompilerProbeRecord {
		return nil, errors.New("compiler probe diagnostic artifact exceeds size limit")
	}
	return data, nil
}

func validateUniqueJSONKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := uniqueJSONValue(dec); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing JSON value")
	}
	return nil
}

func uniqueJSONValue(dec *json.Decoder) error {
	token, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for dec.More() {
			keyToken, err := dec.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			if seen[key] {
				return fmt.Errorf("duplicate JSON field %q", key)
			}
			seen[key] = true
			if err := uniqueJSONValue(dec); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	case '[':
		for dec.More() {
			if err := uniqueJSONValue(dec); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	default:
		return errors.New("unexpected JSON delimiter")
	}
}
