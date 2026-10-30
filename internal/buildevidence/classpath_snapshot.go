package buildevidence

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	ClasspathSnapshotSchema = "codegraph.kotlin_task_classpath_snapshot/v1"
	MaxClasspathEntries     = 10000
	MaxClasspathFiles       = 50000
	MaxClasspathBytes       = int64(1 << 30)
	MaxClasspathFileBytes   = int64(256 << 20)
)

type ClasspathEntry struct {
	Order           int    `json:"order"`
	Kind            string `json:"kind"`
	Role            string `json:"role"`
	State           State  `json:"state"`
	ProvenanceState State  `json:"provenance_state"`
	Scope           string `json:"scope,omitempty"`
	Path            string `json:"path,omitempty"`
	Identity        string `json:"identity,omitempty"`
	SHA256          string `json:"sha256,omitempty"`
	Reason          string `json:"reason,omitempty"`
	Provenance      string `json:"provenance"`
}

// ClasspathSnapshot fingerprints the files declared by one selected
// KotlinCompile task. It does not attest that the compiler consumed them.
type ClasspathSnapshot struct {
	Schema                   string           `json:"schema"`
	Producer                 Producer         `json:"producer"`
	RepositoryIdentity       string           `json:"repository_identity"`
	BuildRoot                string           `json:"build_root"`
	GradleProject            string           `json:"gradle_project"`
	KotlinCompilation        string           `json:"kotlin_compilation"`
	CompileTask              string           `json:"compile_task"`
	V2InputFingerprint       string           `json:"v2_input_fingerprint"`
	ObservationSource        string           `json:"observation_source"`
	ObservationState         State            `json:"observation_state"`
	EntryProvenanceState     State            `json:"entry_provenance_state"`
	ObservationProvenance    []string         `json:"observation_provenance"`
	DependencySelectionState State            `json:"dependency_selection_freshness"`
	CompilerConsumedState    State            `json:"compiler_consumed_classpath"`
	Entries                  []ClasspathEntry `json:"entries"`
	SnapshotSHA256           string           `json:"snapshot_sha256"`
}

type rawClasspathEntry struct {
	Path     string `json:"path"`
	Identity string `json:"identity,omitempty"`
}

type directoryEntry struct {
	path   string
	digest string
	size   int64
}

type SnapshotFreshness struct {
	BuildEvidence   State    `json:"build_evidence_freshness"`
	RecordedContent State    `json:"recorded_content_freshness"`
	Selection       State    `json:"dependency_selection_freshness"`
	CompilerClosure State    `json:"compiler_consumed_classpath"`
	Reasons         []string `json:"reasons"`
}

func buildClasspathSnapshot(root, gradleHome string, artifact Artifact, raw []rawClasspathEntry, observationState State, provenance []string) (ClasspathSnapshot, error) {
	return buildClasspathSnapshotContext(context.Background(), root, gradleHome, artifact, raw, observationState, provenance)
}

func buildClasspathSnapshotContext(ctx context.Context, root, gradleHome string, artifact Artifact, raw []rawClasspathEntry, observationState State, provenance []string) (ClasspathSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return ClasspathSnapshot{}, err
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return ClasspathSnapshot{}, err
	}
	rootAlias := root
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return ClasspathSnapshot{}, errors.New("repository root cannot be resolved")
	}
	if gradleHome == "" {
		return ClasspathSnapshot{}, errors.New("Gradle user home is unavailable for safe classpath path classification")
	}
	gradleHome, err = filepath.Abs(gradleHome)
	if err != nil {
		return ClasspathSnapshot{}, err
	}
	gradleHomeAlias := gradleHome
	gradleHome, err = filepath.EvalSymlinks(gradleHome)
	if err != nil {
		return ClasspathSnapshot{}, errors.New("Gradle user home cannot be resolved")
	}
	for i := range raw {
		absolute, err := filepath.Abs(raw[i].Path)
		if err != nil {
			continue
		}
		if rel, ok := relativeWithin(rootAlias, absolute); ok {
			raw[i].Path = filepath.Join(root, rel)
		} else if rel, ok := relativeWithin(gradleHomeAlias, absolute); ok {
			raw[i].Path = filepath.Join(gradleHome, rel)
		} else {
			raw[i].Path = absolute
		}
	}
	repositoryRoot, err := os.OpenRoot(root)
	if err != nil {
		return ClasspathSnapshot{}, errors.New("repository root cannot be opened safely")
	}
	defer repositoryRoot.Close()
	gradleRoot, err := os.OpenRoot(gradleHome)
	if err != nil {
		return ClasspathSnapshot{}, errors.New("Gradle user home cannot be opened safely")
	}
	defer gradleRoot.Close()
	snapshot := ClasspathSnapshot{
		Schema:                   ClasspathSnapshotSchema,
		Producer:                 Producer{Name: "codegraph-gradle-classpath-snapshot", Version: "1"},
		RepositoryIdentity:       artifact.Repository.Identity,
		BuildRoot:                artifact.Repository.BuildRoot,
		GradleProject:            artifact.Compilation.GradleProject,
		KotlinCompilation:        artifact.Compilation.KotlinCompilation,
		CompileTask:              artifact.Compilation.GradleTask,
		V2InputFingerprint:       artifact.InputFingerprint,
		ObservationSource:        "KotlinCompile.libraries",
		ObservationState:         observationState,
		EntryProvenanceState:     Complete,
		ObservationProvenance:    append([]string(nil), provenance...),
		DependencySelectionState: Unknown,
		CompilerConsumedState:    Unknown,
		Entries:                  make([]ClasspathEntry, 0, len(raw)),
	}
	if observationState != Complete {
		snapshot.ObservationState = Incomplete
	}
	if len(raw) > MaxClasspathEntries {
		raw = raw[:MaxClasspathEntries]
		snapshot.ObservationState = Incomplete
		snapshot.ObservationProvenance = append(snapshot.ObservationProvenance, "classpath entry limit exceeded; remaining entries omitted")
	}
	var totalBytes int64
	for i, entry := range raw {
		if err := ctx.Err(); err != nil {
			return ClasspathSnapshot{}, err
		}
		item := ClasspathEntry{Order: i, State: Complete, Provenance: "selected KotlinCompile.libraries entry"}
		item.Identity = safeClasspathIdentity(entry.Identity)
		path, scope, err := classpathLocator(root, gradleHome, entry.Path)
		if err != nil {
			item.Kind, item.State, item.Reason = "unknown", Incomplete, safeReason(err.Error())
			item.Role, item.ProvenanceState = "unknown", Incomplete
			snapshot.EntryProvenanceState = Incomplete
			snapshot.Entries = append(snapshot.Entries, item)
			snapshot.ObservationState = Incomplete
			continue
		}
		item.Scope, item.Path = scope, path
		baseRoot := repositoryRoot
		if scope == "gradle_user_home" {
			baseRoot = gradleRoot
		}
		info, err := baseRoot.Lstat(filepath.FromSlash(path))
		if err != nil {
			item.Kind, item.State, item.Reason = "unknown", Incomplete, "classpath path cannot be inspected"
			item.Role, item.ProvenanceState = "unknown", Incomplete
			snapshot.EntryProvenanceState = Incomplete
		} else if info.Mode()&os.ModeSymlink != 0 {
			item.Kind, item.State, item.Reason = "unknown", Incomplete, "classpath symlink is not snapshot-supported"
		} else if info.Mode().IsRegular() {
			item.Kind = "file"
			var size int64
			item.SHA256, size, err = stableFileDigestRootContext(ctx, baseRoot, filepath.FromSlash(path), MaxClasspathFileBytes)
			if ctx.Err() != nil {
				return ClasspathSnapshot{}, ctx.Err()
			}
			if err == nil && totalBytes+size > MaxClasspathBytes {
				err = errors.New("classpath snapshot byte limit exceeded")
			}
			if err != nil {
				item.State, item.Reason = Incomplete, safeReason(err.Error())
				item.SHA256 = ""
			} else {
				totalBytes += size
			}
		} else if info.IsDir() {
			item.Kind = "directory"
			var size int64
			item.SHA256, size, err = stableDirectoryDigestRootContext(ctx, baseRoot, filepath.FromSlash(path), MaxClasspathFiles, MaxClasspathBytes-totalBytes)
			if ctx.Err() != nil {
				return ClasspathSnapshot{}, ctx.Err()
			}
			if err != nil || totalBytes+size > MaxClasspathBytes {
				if err == nil {
					err = errors.New("classpath snapshot byte limit exceeded")
				}
				item.State, item.Reason = Incomplete, safeReason(err.Error())
				item.SHA256 = ""
			} else {
				totalBytes += size
			}
		} else {
			item.Kind, item.State, item.Reason = "unknown", Incomplete, "classpath entry is neither a regular file nor a directory"
		}
		item.Role = classifyClasspathRole(item.Scope, item.Path, item.Kind, item.Identity)
		item.ProvenanceState = Complete
		if item.Role == "unknown" {
			item.ProvenanceState = Incomplete
			snapshot.EntryProvenanceState = Incomplete
		}
		if item.State != Complete {
			snapshot.ObservationState = Incomplete
		}
		snapshot.Entries = append(snapshot.Entries, item)
	}
	digest, err := snapshotDigest(snapshot)
	if err != nil {
		return ClasspathSnapshot{}, err
	}
	snapshot.SnapshotSHA256 = digest
	return snapshot, nil
}

func (s ClasspathSnapshot) Validate() error {
	if s.Schema != ClasspathSnapshotSchema || s.Producer.Name != "codegraph-gradle-classpath-snapshot" || s.Producer.Version != "1" {
		return errors.New("unsupported dependency classpath snapshot schema or producer")
	}
	if !strings.HasPrefix(s.RepositoryIdentity, "sha256:") || !isSHA256(strings.TrimPrefix(s.RepositoryIdentity, "sha256:")) || !isSHA256(s.V2InputFingerprint) {
		return errors.New("dependency snapshot repository or v2 fingerprint is invalid")
	}
	if err := validateRepoPath(s.BuildRoot); err != nil {
		return fmt.Errorf("invalid dependency snapshot build root: %w", err)
	}
	if !validGradleProject(s.GradleProject) || !validCompilation(s.KotlinCompilation) || !validGradleProject(s.CompileTask) {
		return errors.New("dependency snapshot selected Gradle project, compilation, and task are required")
	}
	if s.ObservationSource != "KotlinCompile.libraries" || len(s.ObservationProvenance) == 0 || s.Entries == nil {
		return errors.New("dependency snapshot must identify its task classpath observation and explicit entries")
	}
	allowedProvenance := map[string]bool{
		"KotlinCompile.libraries FileCollection rendered through Gradle asPath in iteration order": true,
		"Gradle classpath rendering did not match the Kotlin task FileCollection membership":       true,
		"resolved component identity mapping unavailable; task classpath files remain observable":  true,
		"selected Kotlin task libraries could not be enumerated safely":                            true,
		"classpath entry limit exceeded; remaining entries omitted":                                true,
		"classpath path or model size limit exceeded; remaining entries omitted":                   true,
	}
	for _, provenance := range s.ObservationProvenance {
		if !allowedProvenance[provenance] {
			return errors.New("dependency snapshot contains unsupported observation provenance")
		}
	}
	if s.DependencySelectionState != Unknown || s.CompilerConsumedState != Unknown {
		return errors.New("dependency snapshot cannot promote selection or compiler-consumed completeness")
	}
	if s.ObservationState != Complete && s.ObservationState != Incomplete {
		return errors.New("dependency snapshot observation state must be complete or incomplete")
	}
	if s.EntryProvenanceState != Complete && s.EntryProvenanceState != Incomplete && s.EntryProvenanceState != Unknown {
		return errors.New("dependency snapshot entry provenance state is invalid")
	}
	if len(s.Entries) > MaxClasspathEntries {
		return errors.New("dependency snapshot exceeds entry limit")
	}
	allComplete := true
	for i, entry := range s.Entries {
		if entry.Order != i || entry.Provenance != "selected KotlinCompile.libraries entry" {
			return fmt.Errorf("dependency snapshot entry %d has invalid order or provenance", i)
		}
		if entry.ProvenanceState != Complete && entry.ProvenanceState != Incomplete && entry.ProvenanceState != Unknown {
			return fmt.Errorf("dependency snapshot entry %d has invalid provenance state", i)
		}
		if entry.Identity != "" && safeClasspathIdentity(entry.Identity) != entry.Identity {
			return fmt.Errorf("dependency snapshot entry %d contains an unsafe component identity", i)
		}
		if len(entry.Identity) > 512 {
			return fmt.Errorf("dependency snapshot entry %d component identity exceeds the size limit", i)
		}
		if entry.Role != "external_jar" && entry.Role != "local_file_dependency" && entry.Role != "project_dependency_output" && entry.Role != "classpath_directory" && entry.Role != "unknown" {
			return fmt.Errorf("dependency snapshot entry %d has unsupported role", i)
		}
		if entry.Reason != "" && !validClasspathReason(entry.Reason) {
			return fmt.Errorf("dependency snapshot entry %d contains unsupported diagnostic text", i)
		}
		switch entry.State {
		case Complete:
			if entry.Scope != "repository" && entry.Scope != "gradle_user_home" {
				return fmt.Errorf("dependency snapshot entry %d has unsupported path scope", i)
			}
			if err := validateClasspathLocator(entry.Path); err != nil {
				return fmt.Errorf("dependency snapshot entry %d has unsafe path: %w", i, err)
			}
			if entry.Path == "." {
				return fmt.Errorf("dependency snapshot entry %d cannot name an allowed root", i)
			}
			if entry.Kind != "file" && entry.Kind != "directory" || !isSHA256(entry.SHA256) || entry.Reason != "" {
				return fmt.Errorf("dependency snapshot entry %d has invalid content identity", i)
			}
			if entry.Role == "external_jar" && (entry.Kind != "file" || !strings.HasSuffix(strings.ToLower(entry.Path), ".jar")) || entry.Role == "local_file_dependency" && entry.Kind != "file" || entry.Role == "classpath_directory" && entry.Kind != "directory" {
				return fmt.Errorf("dependency snapshot entry %d role conflicts with its content type", i)
			}
			if entry.Role == "external_jar" && !strings.HasPrefix(entry.Identity, "module:") || entry.Role == "local_file_dependency" && entry.Identity != "file" || entry.Role == "project_dependency_output" && !strings.HasPrefix(entry.Identity, "project:") {
				return fmt.Errorf("dependency snapshot entry %d role lacks matching Gradle component provenance", i)
			}
			if entry.Role == "unknown" && entry.ProvenanceState == Complete {
				return fmt.Errorf("dependency snapshot entry %d claims unknown role provenance is complete", i)
			}
		case Incomplete, Unknown:
			allComplete = false
			if entry.Reason == "" || entry.SHA256 != "" {
				return fmt.Errorf("incomplete dependency snapshot entry %d must carry a reason and no digest", i)
			}
		default:
			return fmt.Errorf("dependency snapshot entry %d has unsupported state %q", i, entry.State)
		}
	}
	if s.ObservationState == Complete && !allComplete {
		return errors.New("complete dependency snapshot contains an incomplete entry")
	}
	if s.EntryProvenanceState == Complete {
		for _, entry := range s.Entries {
			if entry.ProvenanceState != Complete {
				return errors.New("complete entry provenance contains an incomplete entry")
			}
		}
	}
	want, err := snapshotDigest(s)
	if err != nil {
		return err
	}
	if want != s.SnapshotSHA256 {
		return errors.New("dependency snapshot digest mismatch")
	}
	return nil
}

func validClasspathReason(reason string) bool {
	switch reason {
	case "classpath path is outside allowed roots",
		"classpath symlink is unsupported or escapes an allowed root",
		"classpath entry could not be identified safely",
		"classpath path cannot be inspected",
		"classpath symlink is not snapshot-supported",
		"classpath entry is neither a regular file nor a directory",
		"classpath snapshot limit exceeded",
		"classpath content was unstable while hashing",
		"classpath content could not be read or hashed",
		"unsupported classpath entry type":
		return true
	default:
		return false
	}
}

func (s ClasspathSnapshot) Marshal() ([]byte, error) {
	digest, err := snapshotDigest(s)
	if err != nil {
		return nil, err
	}
	s.SnapshotSHA256 = digest
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(s)
}

func DecodeClasspathSnapshot(data []byte) (ClasspathSnapshot, error) {
	if len(data) > MaxArtifactSize {
		return ClasspathSnapshot{}, fmt.Errorf("dependency classpath snapshot exceeds %d bytes", MaxArtifactSize)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var snapshot ClasspathSnapshot
	if err := dec.Decode(&snapshot); err != nil {
		return ClasspathSnapshot{}, fmt.Errorf("decode dependency classpath snapshot: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return ClasspathSnapshot{}, errors.New("dependency classpath snapshot contains trailing JSON")
	}
	if err := snapshot.Validate(); err != nil {
		return ClasspathSnapshot{}, err
	}
	return snapshot, nil
}

// ValidateClasspathSnapshotCurrent rehashes only the paths recorded by the
// sidecar. It cannot re-resolve Gradle dependency selection or prove compiler
// consumption; both remain UNKNOWN in the returned report.
func ValidateClasspathSnapshotCurrent(repositoryRoot, gradleUserHome string, artifact Artifact, snapshot ClasspathSnapshot) (SnapshotFreshness, error) {
	return ValidateClasspathSnapshotCurrentContext(context.Background(), repositoryRoot, gradleUserHome, artifact, snapshot)
}

func ValidateClasspathSnapshotCurrentContext(ctx context.Context, repositoryRoot, gradleUserHome string, artifact Artifact, snapshot ClasspathSnapshot) (SnapshotFreshness, error) {
	if err := ctx.Err(); err != nil {
		return SnapshotFreshness{}, err
	}
	if err := artifact.Validate(); err != nil {
		return SnapshotFreshness{}, fmt.Errorf("validate related v2 evidence: %w", err)
	}
	if err := snapshot.Validate(); err != nil {
		return SnapshotFreshness{}, err
	}
	root, err := filepath.EvalSymlinks(repositoryRoot)
	if err != nil {
		return SnapshotFreshness{}, fmt.Errorf("resolve repository root: %w", err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return SnapshotFreshness{}, err
	}
	home, err := filepath.Abs(gradleUserHome)
	if err != nil {
		return SnapshotFreshness{}, err
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		return SnapshotFreshness{}, fmt.Errorf("resolve Gradle user home: %w", err)
	}
	repositoryRootFS, err := os.OpenRoot(root)
	if err != nil {
		return SnapshotFreshness{}, errors.New("repository root cannot be opened safely")
	}
	defer repositoryRootFS.Close()
	gradleRootFS, err := os.OpenRoot(home)
	if err != nil {
		return SnapshotFreshness{}, errors.New("Gradle user home cannot be opened safely")
	}
	defer gradleRootFS.Close()
	wantIdentity := sha256.Sum256([]byte(root))
	if snapshot.RepositoryIdentity != "sha256:"+hex.EncodeToString(wantIdentity[:]) {
		return SnapshotFreshness{}, errors.New("dependency snapshot belongs to a different repository root")
	}
	if snapshot.RepositoryIdentity != artifact.Repository.Identity || snapshot.BuildRoot != artifact.Repository.BuildRoot || snapshot.GradleProject != artifact.Compilation.GradleProject || snapshot.KotlinCompilation != artifact.Compilation.KotlinCompilation || snapshot.CompileTask != artifact.Compilation.GradleTask || snapshot.V2InputFingerprint != artifact.InputFingerprint {
		return SnapshotFreshness{}, errors.New("dependency snapshot does not match its related v2 evidence artifact")
	}
	result := SnapshotFreshness{BuildEvidence: Incomplete, RecordedContent: Complete, Selection: Unknown, CompilerClosure: Unknown, Reasons: []string{}}
	if err := ctx.Err(); err != nil {
		return SnapshotFreshness{}, err
	}
	if fingerprint, err := CurrentFingerprint(root, artifact); err != nil {
		result.Reasons = append(result.Reasons, "related v2 build evidence could not be revalidated")
	} else if fingerprint != artifact.InputFingerprint {
		result.Reasons = append(result.Reasons, "related v2 build evidence is stale")
	} else {
		result.BuildEvidence = Complete
	}
	if snapshot.ObservationState != Complete {
		result.RecordedContent = Incomplete
		result.Reasons = append(result.Reasons, "original classpath observation was incomplete")
	}
	for _, entry := range snapshot.Entries {
		if err := ctx.Err(); err != nil {
			return SnapshotFreshness{}, err
		}
		if entry.State != Complete {
			result.RecordedContent = Incomplete
			result.Reasons = append(result.Reasons, fmt.Sprintf("entry %d was not snapshotted: %s", entry.Order, entry.Reason))
			continue
		}
		base := repositoryRootFS
		if entry.Scope == "gradle_user_home" {
			base = gradleRootFS
		}
		digest, _, err := digestEntryRootContext(ctx, base, filepath.FromSlash(entry.Path), entry.Kind)
		if ctx.Err() != nil {
			return SnapshotFreshness{}, ctx.Err()
		}
		if err != nil {
			result.RecordedContent = Incomplete
			result.Reasons = append(result.Reasons, fmt.Sprintf("entry %d cannot be revalidated: %s", entry.Order, safeReason(err.Error())))
			continue
		}
		if digest != entry.SHA256 {
			result.RecordedContent = Incomplete
			result.Reasons = append(result.Reasons, fmt.Sprintf("entry %d content changed", entry.Order))
		}
	}
	return result, nil
}

func gradleUserHome() string {
	if configured := os.Getenv("GRADLE_USER_HOME"); configured != "" {
		if absolute, err := filepath.Abs(configured); err == nil {
			return absolute
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".gradle")
	}
	return ""
}

func classpathLocator(root, gradleHome, rawPath string) (string, string, error) {
	if rawPath == "" {
		return "", "", errors.New("classpath entry path is empty")
	}
	logical, err := filepath.Abs(rawPath)
	if err != nil {
		return "", "", errors.New("classpath path cannot be made absolute")
	}
	if info, err := os.Lstat(logical); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", "", errors.New("classpath entry is a symlink")
	}
	resolved, err := filepath.EvalSymlinks(logical)
	if err != nil {
		return "", "", errors.New("classpath path cannot be resolved")
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", "", errors.New("classpath path cannot be made absolute")
	}
	for _, base := range []struct{ path, scope string }{{root, "repository"}, {gradleHome, "gradle_user_home"}} {
		if _, ok := relativeWithin(base.path, resolved); ok {
			if rel, lexicalOK := relativeWithin(base.path, logical); lexicalOK {
				if rel == "." {
					return "", "", errors.New("classpath entry cannot be an allowed root")
				}
				locator := filepath.ToSlash(rel)
				if err := validateClasspathLocator(locator); err != nil {
					return "", "", errors.New("classpath path is not a safe relative locator")
				}
				return locator, base.scope, nil
			}
		}
	}
	return "", "", errors.New("classpath path is outside the repository and Gradle user home")
}

func validateClasspathLocator(value string) error {
	if value == "" || value == "." || len(value) > 4096 {
		return errors.New("classpath locator exceeds size limit")
	}
	if err := validateRepoPath(value); err != nil {
		return err
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return errors.New("classpath locator contains control characters")
		}
	}
	return nil
}

func classifyClasspathRole(scope, path, kind, identity string) string {
	if strings.HasPrefix(identity, "project:") {
		return "project_dependency_output"
	}
	if identity == "file" && kind == "file" {
		return "local_file_dependency"
	}
	if kind == "directory" {
		return "classpath_directory"
	}
	if identity != "" && strings.HasPrefix(identity, "module:") && strings.HasSuffix(strings.ToLower(path), ".jar") {
		return "external_jar"
	}
	return "unknown"
}

func stableFileDigest(path string, limit int64) (string, int64, error) {
	return stableFileDigestContext(context.Background(), path, limit)
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(data []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(data)
}

func stableFileDigestContext(ctx context.Context, path string, limit int64) (string, int64, error) {
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}
	before, err := os.Lstat(path)
	if err != nil {
		return "", 0, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return "", 0, errors.New("classpath file changed type or is a symlink")
	}
	if before.Size() > limit {
		return "", 0, errors.New("classpath file exceeds per-file size limit")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", 0, errors.New("classpath file cannot be read")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return "", 0, errors.New("classpath file changed before hashing")
	}
	digest := sha256.New()
	written, err := io.Copy(digest, io.LimitReader(contextReader{ctx: ctx, r: f}, limit+1))
	if err != nil {
		return "", 0, errors.New("classpath file hashing failed")
	}
	if written > limit {
		return "", 0, errors.New("classpath file exceeds per-file size limit")
	}
	after, err := f.Stat()
	pathAfter, pathErr := os.Lstat(path)
	if err != nil || pathErr != nil || !os.SameFile(opened, after) || !os.SameFile(opened, pathAfter) || opened.Size() != after.Size() || after.Size() != written || !opened.ModTime().Equal(after.ModTime()) {
		return "", 0, errors.New("classpath file changed while hashing")
	}
	return hex.EncodeToString(digest.Sum(nil)), written, nil
}

// stableFileDigestRootContext hashes a relative path through an os.Root opened
// on the approved repository or Gradle home. Root.Open prevents a concurrent
// parent symlink swap from redirecting the read outside that approved tree.
func stableFileDigestRootContext(ctx context.Context, root *os.Root, path string, limit int64) (string, int64, error) {
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}
	before, err := root.Lstat(path)
	if err != nil {
		return "", 0, classifyPathReadError(err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return "", 0, errors.New("classpath file changed type or is a symlink")
	}
	if before.Size() > limit {
		return "", 0, errors.New("classpath file exceeds per-file size limit")
	}
	f, err := root.Open(path)
	if err != nil {
		return "", 0, classifyPathReadError(err)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return "", 0, errors.New("classpath file changed before hashing")
	}
	digest := sha256.New()
	written, err := io.Copy(digest, io.LimitReader(contextReader{ctx: ctx, r: f}, limit+1))
	if err != nil {
		return "", 0, errors.New("classpath file hashing failed")
	}
	if written > limit {
		return "", 0, errors.New("classpath file exceeds per-file size limit")
	}
	after, err := f.Stat()
	pathAfter, pathErr := root.Lstat(path)
	if err != nil || pathErr != nil || !os.SameFile(opened, after) || !os.SameFile(opened, pathAfter) || opened.Size() != after.Size() || after.Size() != written || !opened.ModTime().Equal(after.ModTime()) {
		return "", 0, errors.New("classpath file changed while hashing")
	}
	return hex.EncodeToString(digest.Sum(nil)), written, nil
}

func classifyPathReadError(err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return errors.New("classpath path is missing")
	case errors.Is(err, fs.ErrPermission):
		return errors.New("classpath path cannot be read")
	default:
		return errors.New("classpath path cannot be read within its approved root")
	}
}

func stableDirectoryDigest(root string, maxFiles int, maxBytes int64) (string, int64, error) {
	return stableDirectoryDigestContext(context.Background(), root, maxFiles, maxBytes)
}

func stableDirectoryDigestContext(ctx context.Context, root string, maxFiles int, maxBytes int64) (string, int64, error) {
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return "", 0, errors.New("classpath directory cannot be identified safely")
	}
	items := make([]directoryEntry, 0)
	var total int64
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return errors.New("classpath directory cannot be read")
		}
		if path == root {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("classpath directory contains a symlink")
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("classpath directory contains an unsupported entry")
		}
		if len(items) >= maxFiles {
			return errors.New("classpath directory file-count limit exceeded")
		}
		digest, size, err := stableFileDigestContext(ctx, path, MaxClasspathFileBytes)
		if err != nil {
			return err
		}
		if size > maxBytes-total {
			return errors.New("classpath directory byte limit exceeded")
		}
		total += size
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return errors.New("classpath directory relative path failed")
		}
		items = append(items, directoryEntry{filepath.ToSlash(rel), digest, size})
		return nil
	})
	if err != nil {
		return "", 0, err
	}
	// Rewalk after hashing so additions/removals during the snapshot cannot
	// produce a complete digest from an ambiguous directory state.
	var current []string
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return errors.New("classpath directory changed while hashing")
		}
		if path != root && !entry.IsDir() {
			if entry.Type()&os.ModeSymlink != 0 {
				return errors.New("classpath directory changed to contain a symlink")
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			current = append(current, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return "", 0, err
	}
	want := make([]string, len(items))
	for i := range items {
		want[i] = items[i].path
	}
	sort.Strings(current)
	sort.Strings(want)
	if strings.Join(current, "\x00") != strings.Join(want, "\x00") {
		return "", 0, errors.New("classpath directory membership changed while hashing")
	}
	sort.Slice(items, func(i, j int) bool { return items[i].path < items[j].path })
	for _, item := range items {
		digest, size, err := stableFileDigestContext(ctx, filepath.Join(root, filepath.FromSlash(item.path)), MaxClasspathFileBytes)
		if err != nil || size != item.size || digest != item.digest {
			return "", 0, errors.New("classpath directory content changed while hashing")
		}
	}
	current = current[:0]
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return errors.New("classpath directory changed while hashing")
		}
		if path != root && !entry.IsDir() {
			if entry.Type()&os.ModeSymlink != 0 {
				return errors.New("classpath directory changed to contain a symlink")
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			current = append(current, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return "", 0, err
	}
	sort.Strings(current)
	if strings.Join(current, "\x00") != strings.Join(want, "\x00") {
		return "", 0, errors.New("classpath directory membership changed while hashing")
	}
	rootInfoAfter, err := os.Lstat(root)
	if err != nil || !os.SameFile(rootInfo, rootInfoAfter) {
		return "", 0, errors.New("classpath directory changed while hashing")
	}
	return directoryDigest(items), total, nil
}

func stableDirectoryDigestRootContext(ctx context.Context, root *os.Root, path string, maxFiles int, maxBytes int64) (string, int64, error) {
	rootInfo, err := root.Lstat(path)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return "", 0, errors.New("classpath directory cannot be identified safely")
	}
	dir, err := root.OpenRoot(path)
	if err != nil {
		return "", 0, errors.New("classpath directory cannot be opened within its approved root")
	}
	defer dir.Close()
	openedInfo, err := dir.Stat(".")
	if err != nil || !os.SameFile(rootInfo, openedInfo) {
		return "", 0, errors.New("classpath directory changed before hashing")
	}
	items := make([]directoryEntry, 0)
	var total int64
	err = fs.WalkDir(dir.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return errors.New("classpath directory cannot be read")
		}
		if path == "." {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("classpath directory contains a symlink")
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("classpath directory contains an unsupported entry")
		}
		if len(items) >= maxFiles {
			return errors.New("classpath directory file-count limit exceeded")
		}
		digest, size, err := stableFileDigestRootContext(ctx, dir, path, MaxClasspathFileBytes)
		if err != nil {
			return err
		}
		if size > maxBytes-total {
			return errors.New("classpath directory byte limit exceeded")
		}
		total += size
		items = append(items, directoryEntry{filepath.ToSlash(path), digest, size})
		return nil
	})
	if err != nil {
		return "", 0, err
	}
	var current []string
	err = fs.WalkDir(dir.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return errors.New("classpath directory changed while hashing")
		}
		if path != "." {
			if entry.Type()&os.ModeSymlink != 0 {
				return errors.New("classpath directory changed to contain a symlink")
			}
			if !entry.IsDir() {
				current = append(current, filepath.ToSlash(path))
			}
		}
		return nil
	})
	if err != nil {
		return "", 0, err
	}
	sort.Strings(current)
	if len(current) != len(items) {
		return "", 0, errors.New("classpath directory changed while hashing")
	}
	for i := range items {
		if current[i] != items[i].path {
			return "", 0, errors.New("classpath directory changed while hashing")
		}
	}
	after, err := root.Lstat(path)
	openedAfter, openedErr := dir.Stat(".")
	if err != nil || openedErr != nil || !os.SameFile(rootInfo, after) || !os.SameFile(openedInfo, openedAfter) {
		return "", 0, errors.New("classpath directory changed while hashing")
	}
	return directoryDigest(items), total, nil
}

func directoryDigest(items []directoryEntry) string {
	// This function is intentionally separate from JSON encoding: every value
	// is length framed so delimiters in file names cannot alias another listing.
	h := sha256.New()
	for _, item := range items {
		writeFrame(h, []byte(item.path))
		writeFrame(h, []byte(fmt.Sprint(item.size)))
		writeFrame(h, []byte(item.digest))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func writeFrame(h hash.Hash, value []byte) {
	var size [8]byte
	// Encode the length explicitly without a dependency on architecture width.
	length := uint64(len(value))
	for i := 7; i >= 0; i-- {
		size[i] = byte(length)
		length >>= 8
	}
	_, _ = h.Write(size[:])
	_, _ = h.Write(value)
}

func digestEntry(path, kind string) (string, int64, error) {
	return digestEntryContext(context.Background(), path, kind)
}

func digestEntryContext(ctx context.Context, path, kind string) (string, int64, error) {
	if kind == "file" {
		return stableFileDigestContext(ctx, path, MaxClasspathFileBytes)
	}
	if kind != "directory" {
		return "", 0, errors.New("unsupported recorded classpath entry type")
	}
	return stableDirectoryDigestContext(ctx, path, MaxClasspathFiles, MaxClasspathBytes)
}

func digestEntryRootContext(ctx context.Context, root *os.Root, path, kind string) (string, int64, error) {
	if kind == "file" {
		return stableFileDigestRootContext(ctx, root, path, MaxClasspathFileBytes)
	}
	if kind != "directory" {
		return "", 0, errors.New("unsupported recorded classpath entry type")
	}
	return stableDirectoryDigestRootContext(ctx, root, path, MaxClasspathFiles, MaxClasspathBytes)
}

func snapshotDigest(snapshot ClasspathSnapshot) (string, error) {
	snapshot.SnapshotSHA256 = ""
	data, err := json.Marshal(snapshot)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func safeClasspathIdentity(identity string) string {
	if identity == "" {
		return ""
	}
	if identity == "file" {
		return identity
	}
	if strings.HasPrefix(identity, "module:") && safeDependencyCoordinate(strings.TrimPrefix(identity, "module:")) {
		return identity
	}
	if strings.HasPrefix(identity, "project:") && validGradleProject(strings.TrimPrefix(identity, "project:")) {
		return identity
	}
	return ""
}

func safeReason(reason string) string {
	// Reasons are stable classifications. Never persist host paths or arbitrary
	// file-system error strings supplied by the operating system.
	switch {
	case strings.Contains(reason, "outside the repository"):
		return "classpath path is outside allowed roots"
	case strings.Contains(reason, "symlink"):
		return "classpath symlink is unsupported or escapes an allowed root"
	case strings.Contains(reason, "size limit") || strings.Contains(reason, "byte limit") || strings.Contains(reason, "file-count limit") || strings.Contains(reason, "entry limit"):
		return "classpath snapshot limit exceeded"
	case strings.Contains(reason, "changed") || strings.Contains(reason, "membership"):
		return "classpath content was unstable while hashing"
	case strings.Contains(reason, "cannot be read") || strings.Contains(reason, "hashing failed"):
		return "classpath content could not be read or hashed"
	case strings.Contains(reason, "missing"):
		return "classpath path is missing"
	case strings.Contains(reason, "neither a regular file") || strings.Contains(reason, "unsupported entry"):
		return "unsupported classpath entry type"
	default:
		return "classpath entry could not be identified safely"
	}
}
