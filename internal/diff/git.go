package diff

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/isink17/codegraph/internal/config"
	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/limits"
	"github.com/isink17/codegraph/internal/parser"
	"github.com/isink17/codegraph/internal/store"
)

type treeEntry struct{ mode, object, name string }

const (
	// ponytail: fixed ceilings bound temporary snapshot disk/RAM; raise them only with measured large-repo demand and bounded parsing.
	maxTreeListingBytes       = 64 << 20
	maxSnapshotFiles          = 100_000
	maxSnapshotBytes    int64 = 512 << 20
)

type boundedCapture struct {
	buffer   bytes.Buffer
	limit    int64
	exceeded bool
	cancel   func()
}

func newBoundedCapture(limit int64, cancel func()) *boundedCapture {
	return &boundedCapture{limit: limit, cancel: cancel}
}
func (b *boundedCapture) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - int64(b.buffer.Len())
	if remaining > 0 {
		keep := len(p)
		if int64(keep) > remaining {
			keep = int(remaining)
		}
		_, _ = b.buffer.Write(p[:keep])
	}
	if int64(n) > remaining {
		b.exceeded = true
		if b.cancel != nil {
			b.cancel()
		}
	}
	return n, nil
}

func CompareCommits(ctx context.Context, repoRoot, baseRef, headRef string, registry *parser.Registry, offset, limit int) (Result, error) {
	if registry == nil {
		return Result{}, errors.New("diff: parser registry is required")
	}
	if limit == 0 {
		limit = limits.DefaultPage
	}
	if offset < 0 || limit < 1 || limit > limits.MaxPage {
		return Result{}, fmt.Errorf("invalid diff page: offset must be non-negative and limit must be 1..%d", limits.MaxPage)
	}
	base, err := snapshot(ctx, repoRoot, baseRef, registry)
	if err != nil {
		return Result{}, fmt.Errorf("base revision: %w", err)
	}
	head, err := snapshot(ctx, repoRoot, headRef, registry)
	if err != nil {
		return Result{}, fmt.Errorf("head revision: %w", err)
	}
	return Compare(base, head, offset, limit)
}

func snapshot(ctx context.Context, repoRoot, ref string, registry *parser.Registry) (Revision, error) {
	if strings.TrimSpace(ref) == "" || strings.HasPrefix(ref, "-") {
		return Revision{}, fmt.Errorf("invalid commit reference %q", ref)
	}
	commit, err := gitOutput(ctx, repoRoot, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return Revision{}, fmt.Errorf("resolve local commit %q: %w", ref, err)
	}
	tree, err := gitOutput(ctx, repoRoot, "rev-parse", "--verify", "--end-of-options", commit+"^{tree}")
	if err != nil {
		return Revision{}, err
	}
	listing, err := gitOutputBytesLimited(ctx, repoRoot, maxTreeListingBytes, "ls-tree", "-r", "-z", "--full-tree", commit)
	if err != nil {
		return Revision{}, fmt.Errorf("list commit tree: %w", err)
	}
	entries, err := parseTree(listing)
	if err != nil {
		return Revision{}, err
	}
	tmp, err := os.MkdirTemp("", "codegraph-diff-*")
	if err != nil {
		return Revision{}, err
	}
	defer os.RemoveAll(tmp)
	root := filepath.Join(tmp, "tree")
	if err := os.Mkdir(root, 0o755); err != nil {
		return Revision{}, err
	}
	treeFiles, err := materialize(ctx, repoRoot, commit, entries, root, registry)
	if err != nil {
		return Revision{}, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return Revision{}, err
	}
	repoCfg, err := config.LoadRepo(root)
	if err != nil {
		return Revision{}, err
	}
	policy := IndexPolicy{Include: nonnil(repoCfg.Include), Exclude: nonnil(repoCfg.Exclude), Languages: nonnil(repoCfg.Languages), MaxFileSizeBytes: repoCfg.MaxFileSizeBytes, ParseErrorPolicy: repoCfg.ParseErrorPolicy}
	st, err := store.Open(filepath.Join(tmp, "graph.sqlite"))
	if err != nil {
		return Revision{}, err
	}
	defer st.Close()
	idx := indexer.New(st, registry, nil)
	if _, err := idx.Index(ctx, indexer.Options{RepoRoot: root, Include: repoCfg.Include, Exclude: repoCfg.Exclude, Languages: repoCfg.Languages, NoHistory: true}); err != nil {
		return Revision{}, fmt.Errorf("index commit: %w", err)
	}
	repo, ok, err := st.FindRepo(ctx, root)
	if err != nil {
		return Revision{}, err
	}
	if !ok {
		return Revision{}, errors.New("temporary index did not create repository")
	}
	graph, err := st.SemanticGraph(ctx, repo.ID)
	if err != nil {
		return Revision{}, err
	}
	return Revision{Revision: ref, Commit: commit, Tree: tree, GraphData: graph, TreeFiles: treeFiles, IndexPolicy: policy}, nil
}

func parseTree(raw []byte) ([]treeEntry, error) {
	if len(raw) > maxTreeListingBytes {
		return nil, fmt.Errorf("Git tree listing exceeds %d bytes", maxTreeListingBytes)
	}
	var out []treeEntry
	for _, rec := range bytes.Split(raw, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		meta, name, ok := bytes.Cut(rec, []byte{'\t'})
		if !ok {
			return nil, errors.New("malformed Git tree entry")
		}
		fields := bytes.Fields(meta)
		if len(fields) != 3 {
			return nil, errors.New("malformed Git tree metadata")
		}
		mode, typ, object, n := string(fields[0]), string(fields[1]), string(fields[2]), string(name)
		if !utf8.ValidString(n) {
			return nil, errors.New("Git paths with invalid UTF-8 are unsupported")
		}
		if mode == "160000" || typ == "commit" {
			return nil, errors.New("git submodules (gitlinks) are unsupported")
		}
		if typ != "blob" || mode != "100644" && mode != "100755" && mode != "120000" {
			return nil, fmt.Errorf("unsupported Git tree entry %q (mode %s, type %s)", n, mode, typ)
		}
		clean := path.Clean(n)
		if clean == "." || path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(n, "\\") {
			return nil, fmt.Errorf("unsafe Git path %q", n)
		}
		out = append(out, treeEntry{mode: mode, object: object, name: n})
		if err := validateSnapshotFileCount(len(out)); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func materialize(ctx context.Context, repoRoot, commit string, entries []treeEntry, root string, registry *parser.Registry) ([]SourceFile, error) {
	if err := validateSnapshotFileCount(len(entries)); err != nil {
		return nil, err
	}
	cmd := gitCommand(ctx, repoRoot, "cat-file", "--batch")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	writeErr := make(chan error, 1)
	go func() {
		for _, e := range entries {
			if _, err := io.WriteString(stdin, e.object+"\n"); err != nil {
				_ = stdin.Close()
				writeErr <- err
				return
			}
		}
		writeErr <- stdin.Close()
	}()
	r := bufio.NewReader(stdout)
	files := make([]SourceFile, 0, len(entries))
	var totalBytes int64
	for _, e := range entries {
		header, err := r.ReadString('\n')
		if err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return nil, fmt.Errorf("read Git blob header: %w", err)
		}
		fields := strings.Fields(header)
		if len(fields) != 3 || fields[0] != e.object || fields[1] != "blob" {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return nil, fmt.Errorf("invalid Git blob response %q", strings.TrimSpace(header))
		}
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || size < 0 {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return nil, fmt.Errorf("invalid Git blob size %q", fields[2])
		}
		totalBytes, err = addSnapshotBytes(totalBytes, size)
		if err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return nil, err
		}
		name := path.Clean(e.name)
		dst := filepath.Join(root, filepath.FromSlash(name))
		rel, err := filepath.Rel(root, dst)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return nil, fmt.Errorf("unsafe Git path %q", e.name)
		}
		if err := safeParents(root, filepath.Dir(dst)); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return nil, err
		}
		if e.mode == "120000" {
			if size > 32*1024 {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				return nil, fmt.Errorf("symlink target too long: %q", e.name)
			}
			var target bytes.Buffer
			if _, err := io.CopyN(&target, r, size); err != nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				return nil, err
			}
			if _, err := r.ReadByte(); err != nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				return nil, err
			}
			link := target.String()
			if err := validateSymlink(name, link); err != nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				return nil, err
			}
			if err := os.Symlink(link, dst); err != nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				return nil, err
			}
			files = append(files, sourceFile(e.name, target.Bytes(), registry))
			continue
		}
		mode := os.FileMode(0o644)
		if e.mode == "100755" {
			mode = 0o755
		}
		f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return nil, err
		}
		hasher := sha256.New()
		_, copyErr := io.CopyN(io.MultiWriter(f, hasher), r, size)
		closeErr := f.Close()
		if copyErr != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return nil, copyErr
		}
		if closeErr != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return nil, closeErr
		}
		if _, err := r.ReadByte(); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return nil, err
		}
		files = append(files, SourceFile{Path: e.name, ContentHash: hex.EncodeToString(hasher.Sum(nil)), Language: languageFor(e.name, registry)})
	}
	if err := <-writeErr; err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, err
	}
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("read Git blobs: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return files, nil
}

func validateSymlink(name, target string) error {
	if target == "" || strings.IndexByte(target, 0) >= 0 || strings.Contains(target, "\\") || strings.Contains(strings.SplitN(target, "/", 2)[0], ":") || path.IsAbs(target) || filepath.IsAbs(target) || filepath.VolumeName(target) != "" {
		return fmt.Errorf("unsafe symlink %q", name)
	}
	resolved := path.Clean(path.Join(path.Dir(name), target))
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return fmt.Errorf("symlink escapes commit root: %q", name)
	}
	return nil
}

func safeParents(root, parent string) error {
	rel, err := filepath.Rel(root, parent)
	if err != nil {
		return err
	}
	if rel == "." {
		return nil
	}
	cur := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		st, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			if err = os.Mkdir(cur, 0o755); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("Git path traverses non-directory %q", cur)
		}
	}
	return nil
}

func gitCommand(ctx context.Context, root string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
	env := os.Environ()
	out := env[:0]
	for _, v := range env {
		if !strings.HasPrefix(v, "GIT_NO_LAZY_FETCH=") && !strings.HasPrefix(v, "GIT_NO_REPLACE_OBJECTS=") {
			out = append(out, v)
		}
	}
	cmd.Env = append(out, "GIT_NO_LAZY_FETCH=1", "GIT_NO_REPLACE_OBJECTS=1")
	return cmd
}

func sourceFile(name string, content []byte, registry *parser.Registry) SourceFile {
	sum := sha256.Sum256(content)
	return SourceFile{Path: name, ContentHash: hex.EncodeToString(sum[:]), Language: languageFor(name, registry)}
}

func languageFor(name string, registry *parser.Registry) string {
	if adapter := registry.AdapterFor(name); adapter != nil {
		return adapter.Language()
	}
	return ""
}

func nonnil(in []string) []string {
	if in == nil {
		return []string{}
	}
	return append([]string{}, in...)
}

func gitOutput(ctx context.Context, root string, args ...string) (string, error) {
	out, err := gitOutputBytes(ctx, root, args...)
	return strings.TrimSpace(string(out)), err
}
func gitOutputBytes(ctx context.Context, root string, args ...string) ([]byte, error) {
	return gitOutputBytesLimited(ctx, root, 0, args...)
}
func gitOutputBytesLimited(ctx context.Context, root string, limit int64, args ...string) ([]byte, error) {
	cmd := gitCommand(ctx, root, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	var capture bytes.Buffer
	if limit <= 0 {
		cmd.Stdout = &capture
	} else {
		cmd.Stdout = newBoundedCapture(limit, func() {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		})
	}
	err := cmd.Run()
	if limit > 0 {
		bounded := cmd.Stdout.(*boundedCapture)
		if bounded.exceeded {
			return nil, fmt.Errorf("git %s output exceeds %d bytes", strings.Join(args, " "), limit)
		}
		capture = bounded.buffer
	}
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return capture.Bytes(), nil
}

func validateSnapshotFileCount(count int) error {
	if count > maxSnapshotFiles {
		return fmt.Errorf("snapshot has more than %d files", maxSnapshotFiles)
	}
	return nil
}
func addSnapshotBytes(total, size int64) (int64, error) {
	if total < 0 || size < 0 || total > maxSnapshotBytes-size {
		return 0, fmt.Errorf("snapshot exceeds %d bytes", maxSnapshotBytes)
	}
	return total + size, nil
}
