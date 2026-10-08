package buildevidence

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed gradle.init.gradle
var gradleInitScript embed.FS

// GradleRequest is an explicitly authorized invocation for one Kotlin/JVM
// compilation. GradleCommand must name the executable selected by the user;
// this function never searches for or invokes a project wrapper.
type GradleRequest struct {
	RepositoryRoot string
	GradleCommand  string
	Project        string
	Compilation    string
	OutputPath     string
	Stderr         io.Writer
}

type gradleModel struct {
	GradleVersion       string   `json:"gradle_version"`
	KotlinPluginVersion string   `json:"kotlin_plugin_version"`
	JDKVendor           string   `json:"jdk_vendor"`
	JDKVersion          string   `json:"jdk_version"`
	Project             string   `json:"project"`
	Compilation         string   `json:"compilation"`
	SourceRoots         []string `json:"source_roots"`
	Dependencies        []string `json:"dependencies"`
}

// ExportGradle invokes Gradle only at this explicit call site and writes a
// schema-valid artifact. Dimensions stay incomplete/unknown unless the model
// exposes evidence this first Kotlin/JVM slice can audit.
func ExportGradle(ctx context.Context, req GradleRequest) (Artifact, error) {
	root, err := filepath.Abs(req.RepositoryRoot)
	if err != nil {
		return Artifact{}, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return Artifact{}, fmt.Errorf("resolve repository root: %w", err)
	}
	rootInfo, err := os.Stat(root)
	if err != nil || !rootInfo.IsDir() {
		return Artifact{}, fmt.Errorf("repository root is not a directory: %s", root)
	}
	if req.GradleCommand == "" || req.Project == "" || req.Compilation == "" || req.OutputPath == "" {
		return Artifact{}, errors.New("Gradle command, project, compilation, and output are required")
	}
	if !validGradleProject(req.Project) || !validCompilation(req.Compilation) {
		return Artifact{}, errors.New("invalid Gradle project or Kotlin compilation")
	}
	output, err := filepath.Abs(req.OutputPath)
	if err != nil {
		return Artifact{}, err
	}
	if _, inside := relativeWithin(root, output); inside {
		return Artifact{}, errors.New("artifact output must be outside the repository")
	}

	before, err := repositoryFingerprint(root, nil)
	if err != nil {
		return Artifact{}, fmt.Errorf("fingerprint repository before Gradle: %w", err)
	}
	tmpDir, err := os.MkdirTemp("", "codegraph-gradle-evidence-")
	if err != nil {
		return Artifact{}, err
	}
	defer os.RemoveAll(tmpDir)
	initPath := filepath.Join(tmpDir, "evidence.init.gradle")
	script, err := gradleInitScript.ReadFile("gradle.init.gradle")
	if err != nil {
		return Artifact{}, err
	}
	if err := os.WriteFile(initPath, script, 0o600); err != nil {
		return Artifact{}, err
	}
	modelPath := filepath.Join(tmpDir, "model.json")
	args := []string{
		"--no-daemon", "--init-script", initPath,
		"-Pcg35.project=" + req.Project,
		"-Pcg35.compilation=" + req.Compilation,
		"-Pcg35.output=" + modelPath,
		":codegraphExportEvidence",
	}
	if req.Stderr != nil {
		if _, err := fmt.Fprintf(req.Stderr, "Running explicitly selected Gradle executable for %s/%s\n", req.Project, req.Compilation); err != nil {
			return Artifact{}, err
		}
	}
	cmd := exec.CommandContext(ctx, req.GradleCommand, args...)
	cmd.Dir = root
	cmd.Stdout = req.Stderr
	cmd.Stderr = req.Stderr
	if err := cmd.Run(); err != nil {
		return Artifact{}, fmt.Errorf("run explicitly requested Gradle evidence task: %w", err)
	}
	modelBytes, err := readBoundedFile(modelPath, MaxArtifactSize)
	if err != nil {
		return Artifact{}, fmt.Errorf("read Gradle model output: %w", err)
	}
	var model gradleModel
	dec := json.NewDecoder(bytes.NewReader(modelBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&model); err != nil {
		return Artifact{}, fmt.Errorf("decode Gradle model output: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return Artifact{}, errors.New("Gradle model output has trailing JSON")
	}
	if model.Project != req.Project || model.Compilation != req.Compilation || model.GradleVersion == "" {
		return Artifact{}, errors.New("Gradle model did not match the requested project and compilation")
	}
	if model.SourceRoots == nil {
		return Artifact{}, errors.New("Gradle model omitted Kotlin source roots")
	}
	sort.Strings(model.Dependencies)
	if !safeToolIdentity(model.GradleVersion) || !safeToolIdentity(model.KotlinPluginVersion) || !safeToolIdentity(model.JDKVersion) || !safeToolIdentity(model.JDKVendor) {
		return Artifact{}, errors.New("Gradle reported unsupported tool identity text")
	}
	for _, dependency := range model.Dependencies {
		if !safeDependencyCoordinate(dependency) {
			return Artifact{}, errors.New("Gradle reported a dependency coordinate outside the supported non-secret format")
		}
	}
	roots, err := canonicalRepoRoots(root, model.SourceRoots)
	if err != nil {
		return Artifact{}, err
	}
	model.SourceRoots = roots
	after, err := repositoryFingerprint(root, nil)
	if err != nil {
		return Artifact{}, fmt.Errorf("fingerprint repository after Gradle: %w", err)
	}
	if before != after {
		return Artifact{}, errors.New("repository inputs changed while Gradle was producing evidence; rerun against a stable tree")
	}
	inputs, err := repositoryFingerprint(root, roots)
	if err != nil {
		return Artifact{}, fmt.Errorf("fingerprint selected Kotlin source roots: %w", err)
	}
	identity := sha256.Sum256([]byte(root))
	dimensions := Dimensions{
		Source:           Dimension{State: Incomplete, Provenance: []string{"Gradle Kotlin/JVM compilation source-set roots; generated inputs are not proven"}},
		Generated:        Dimension{State: Unknown, Provenance: []string{"first producer does not enumerate generated source ownership"}},
		Excluded:         Dimension{State: Unknown, Provenance: []string{"first producer does not observe excluded source rules"}},
		Dependency:       Dimension{State: Incomplete, Provenance: []string{"Gradle resolved compilation artifacts observed; file dependencies and artifact content freshness are not proven"}},
		ExternalMetadata: Dimension{State: Unknown, Provenance: []string{"external metadata is not captured"}},
		CompilerIdentity: Dimension{State: Incomplete, Provenance: []string{"Gradle and Kotlin plugin versions observed; Kotlin compiler and selected toolchain identity are not proven"}},
	}
	fingerprint := hashFingerprint(root, model, roots, inputs, dimensions)
	artifact := Artifact{
		Schema:   Schema,
		Producer: Producer{Name: "codegraph-gradle-exporter", Version: "1"},
		Repository: Repository{
			Identity:   "sha256:" + hex.EncodeToString(identity[:]),
			RootMarker: "sha256:" + hex.EncodeToString(identity[:]),
			BuildRoot:  ".",
		},
		Compilation: Compilation{GradleProject: req.Project, KotlinCompilation: req.Compilation},
		Tools: Tools{
			GradleVersion:       model.GradleVersion,
			KotlinPluginVersion: model.KotlinPluginVersion,
			JDKVendor:           model.JDKVendor,
			JDKVersion:          model.JDKVersion,
		},
		InputFingerprint: fingerprint,
		Evidence: Evidence{
			Dimensions:  dimensions,
			SourceRoots: roots,
			Dependencies: func() []Dependency {
				out := make([]Dependency, 0, len(model.Dependencies))
				for _, coordinate := range model.Dependencies {
					out = append(out, Dependency{Coordinate: coordinate, State: "positive", Provenance: "Gradle selected compilation classpath"})
				}
				return out
			}(),
		},
	}
	data, err := artifact.Marshal()
	if err != nil {
		return Artifact{}, err
	}
	if len(data) > MaxArtifactSize {
		return Artifact{}, fmt.Errorf("generated build evidence exceeds %d bytes", MaxArtifactSize)
	}
	if err := writeNewFile(output, data); err != nil {
		return Artifact{}, err
	}
	return artifact, nil
}

// CurrentFingerprint recomputes the locally available inputs for stale-artifact
// checks. It cannot attest Gradle-resolved dependencies, which remain unknown.
func CurrentFingerprint(repositoryRoot string, artifact Artifact) (string, error) {
	if err := artifact.Validate(); err != nil {
		return "", err
	}
	root, err := filepath.Abs(repositoryRoot)
	if err != nil {
		return "", err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	identity := sha256.Sum256([]byte(root))
	want := "sha256:" + hex.EncodeToString(identity[:])
	if artifact.Repository.Identity != want || artifact.Repository.RootMarker != want {
		return "", errors.New("build evidence belongs to a different repository root")
	}
	roots := artifact.Evidence.SourceRoots
	model := gradleModel{
		GradleVersion:       artifact.Tools.GradleVersion,
		KotlinPluginVersion: artifact.Tools.KotlinPluginVersion,
		JDKVendor:           artifact.Tools.JDKVendor,
		JDKVersion:          artifact.Tools.JDKVersion,
		Project:             artifact.Compilation.GradleProject,
		Compilation:         artifact.Compilation.KotlinCompilation,
		SourceRoots:         roots,
	}
	for _, dependency := range artifact.Evidence.Dependencies {
		model.Dependencies = append(model.Dependencies, dependency.Coordinate)
	}
	sort.Strings(model.Dependencies)
	current, err := repositoryFingerprint(root, roots)
	if err != nil {
		return "", err
	}
	return hashFingerprint(root, model, roots, current, artifact.Evidence.Dimensions), nil
}

// ValidateCurrentInRepository recomputes local inputs from the artifact and
// refuses a stale fingerprint without invoking Gradle.
func (a Artifact) ValidateCurrentInRepository(repositoryRoot string) error {
	current, err := CurrentFingerprint(repositoryRoot, a)
	if err != nil {
		return err
	}
	return a.ValidateCurrent(current)
}

func readBoundedFile(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("Gradle model output exceeds %d bytes", limit)
	}
	return data, nil
}

func safeToolIdentity(value string) bool {
	if value == "" {
		return true
	}
	if len(value) > 128 || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("._+- ()", r) {
			continue
		}
		return false
	}
	return true
}

func safeDependencyCoordinate(value string) bool {
	parts := strings.Split(value, ":")
	if len(parts) != 3 {
		if strings.HasPrefix(value, "project ") {
			return validGradleProject(strings.TrimPrefix(value, "project "))
		}
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, r := range part {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("._+-", r) {
				continue
			}
			return false
		}
	}
	return true
}

func canonicalRepoRoots(root string, paths []string) ([]string, error) {
	set := make(map[string]struct{}, len(paths))
	for _, source := range paths {
		if source == "" {
			return nil, errors.New("Gradle reported an empty source root")
		}
		abs, err := filepath.Abs(source)
		if err != nil {
			return nil, err
		}
		rel, err := canonicalRepoPath(root, abs)
		if err != nil {
			return nil, fmt.Errorf("resolve Gradle source root %s: %w", source, err)
		}
		if rel == "." {
			return nil, errors.New("Gradle source root cannot be the repository root")
		}
		set[rel] = struct{}{}
	}
	roots := make([]string, 0, len(set))
	for root := range set {
		roots = append(roots, root)
	}
	sort.Strings(roots)
	return roots, nil
}

func canonicalRepoPath(root, abs string) (string, error) {
	rel, ok := relativeWithin(root, abs)
	if !ok {
		return "", errors.New("path escapes repository")
	}
	parts := strings.Split(rel, string(filepath.Separator))
	current := root
	for i, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			current = filepath.Join(append([]string{current}, parts[i+1:]...)...)
			break
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			current, err = filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			if _, ok := relativeWithin(root, current); !ok {
				return "", errors.New("symlink escapes repository")
			}
		}
	}
	if info, err := os.Stat(current); err == nil && !info.IsDir() {
		return "", errors.New("source root is not a directory")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	rel, err := filepath.Rel(root, current)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(filepath.Clean(rel)), nil
}

// ponytail: hashes the full non-generated tree in O(repository bytes); use a
// Gradle-proven input manifest if explicit freshness checks become costly.
func repositoryFingerprint(root string, sourceRoots []string) (string, error) {
	h := sha256.New()
	write := func(name string, data []byte) {
		h.Write([]byte(name))
		h.Write([]byte{0})
		h.Write(data)
		h.Write([]byte{0})
	}
	writeFile := func(name, path string) error {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		h.Write([]byte(name))
		h.Write([]byte{0})
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return err
		}
		_, _ = fmt.Fprintf(h, "%o", info.Mode().Perm())
		h.Write([]byte{0})
		_, copyErr := io.Copy(h, f)
		closeErr := f.Close()
		h.Write([]byte{0})
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	}
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", ".gradle", "build", ".codegraph":
				if name != root {
					return filepath.SkipDir
				}
			}
			return nil
		}
		var info fs.FileInfo
		var err error
		linkTarget := ""
		if entry.Type()&os.ModeSymlink != 0 {
			target, err := filepath.EvalSymlinks(name)
			if err != nil {
				return fmt.Errorf("resolve repository symlink %s: %w", name, err)
			}
			if _, ok := relativeWithin(root, target); !ok {
				return fmt.Errorf("repository symlink escapes repository: %s", name)
			}
			linkTarget, err = filepath.Rel(root, target)
			if err != nil {
				return err
			}
			info, err = os.Stat(name)
			if err != nil {
				return err
			}
			if info.IsDir() {
				return fmt.Errorf("directory symlink prevents a complete local fingerprint: %s", name)
			}
		} else {
			info, err = entry.Info()
			if err != nil {
				return err
			}
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		if linkTarget != "" {
			write("symlink:"+filepath.ToSlash(rel), []byte(filepath.ToSlash(linkTarget)))
		}
		return writeFile(filepath.ToSlash(rel), name)
	})
	if err != nil {
		return "", err
	}
	for _, sourceRoot := range sourceRoots {
		abs := filepath.Join(root, filepath.FromSlash(sourceRoot))
		write("source-root:"+sourceRoot, nil)
		err := filepath.WalkDir(abs, func(name string, entry fs.DirEntry, walkErr error) error {
			if errors.Is(walkErr, os.ErrNotExist) && name == abs {
				return nil
			}
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || !entry.Type().IsRegular() {
				return nil
			}
			rel, err := filepath.Rel(root, name)
			if err != nil {
				return err
			}
			return writeFile("source:"+filepath.ToSlash(rel), name)
		})
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func hashFingerprint(root string, model gradleModel, roots []string, repositoryHash string, dimensions Dimensions) string {
	data, _ := json.Marshal(struct {
		Repository string      `json:"repository"`
		Model      gradleModel `json:"model"`
		Roots      []string    `json:"roots"`
		Inputs     string      `json:"inputs"`
		Dimensions Dimensions  `json:"dimensions"`
	}{root, model, roots, repositoryHash, dimensions})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func relativeWithin(root, path string) (string, bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	return rel, true
}

func writeNewFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create evidence artifact without replacing existing files: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return err
	}
	return nil
}
