package indexer

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/isink17/codegraph/internal/platform"
)

type swiftTarget struct {
	name  string
	root  string
	paths []string
}

type swiftModuleMap struct {
	files       map[string]string
	packages    map[string]string
	manifests   bool
	fingerprint string
	swiftFiles  []string
}

func (m swiftModuleMap) moduleFor(rel string) string {
	keys := make([]string, 0, len(m.files))
	for prefix := range m.files {
		keys = append(keys, prefix)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	for _, prefix := range keys {
		module := m.files[prefix]
		if strings.HasSuffix(prefix, "/") && strings.HasPrefix(rel, prefix) {
			return module
		}
		if rel == prefix {
			return module
		}
	}
	return ""
}

func (m swiftModuleMap) packageFor(rel string) string {
	keys := make([]string, 0, len(m.packages))
	for prefix := range m.packages {
		keys = append(keys, prefix)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	for _, prefix := range keys {
		root := strings.TrimSuffix(prefix, "/")
		if root == "" || rel == root || strings.HasPrefix(rel, root+"/") {
			return m.packages[prefix]
		}
	}
	return ""
}

var swiftTargetCall = regexp.MustCompile(`\.(target|executableTarget|testTarget|plugin|macro)\s*\(`)

func discoverSwiftModules(root string) (swiftModuleMap, error) {
	m := swiftModuleMap{files: map[string]string{}, packages: map[string]string{}}
	var manifests []string
	err := walkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".swift") {
			rel, e := filepath.Rel(root, path)
			if e != nil {
				return e
			}
			logical, e := platform.NativeRelativeToLogical(filepath.Clean(rel))
			if e != nil {
				return e
			}
			m.swiftFiles = append(m.swiftFiles, logical)
		}
		if !d.IsDir() && d.Name() == "Package.swift" {
			manifests = append(manifests, path)
		}
		return nil
	})
	if err != nil {
		return m, err
	}
	sort.Strings(manifests)
	h := sha256.New()
	h.Write([]byte("swiftpm-manifests-v1\x00"))
	for _, manifest := range manifests {
		data, err := os.ReadFile(manifest)
		if err != nil {
			return m, err
		}
		pkgRoot, err := filepath.Rel(root, filepath.Dir(manifest))
		if err != nil {
			return m, err
		}
		if filepath.Clean(pkgRoot) == "." {
			pkgRoot = ""
		} else {
			pkgRoot, err = platform.NativeRelativeToLogical(filepath.Clean(pkgRoot))
			if err != nil {
				return m, err
			}
		}
		if pkgRoot == "." {
			pkgRoot = ""
		}
		packageScope := pkgRoot
		if packageScope == "" {
			packageScope = "."
		}
		manifestRel, err := filepath.Rel(root, manifest)
		if err != nil {
			return m, err
		}
		manifestRel, err = platform.NativeRelativeToLogical(filepath.Clean(manifestRel))
		if err != nil {
			return m, err
		}
		writeSwiftFingerprintField(h, manifestRel)
		writeSwiftFingerprintField(h, string(data))
		for _, target := range parseSwiftTargets(string(data)) {
			if !target.excludesKnown {
				continue
			}
			base := target.path
			if base == "" {
				base = "Sources/" + target.name
				if target.kind == "test" {
					base = "Tests/" + target.name
				}
				if target.kind == "plugin" {
					base = "Plugins/" + target.name
				}
			}
			base = pathpkg.Clean(pathpkg.Join(pkgRoot, base))
			m.packages[base+"/"] = packageScope
			m.files[base+"/"] = target.name
			if target.sourcesKnown {
				target.paths = target.sources
			}
			if !target.sourcesKnown {
				for _, file := range m.swiftFiles {
					if file == base || strings.HasPrefix(file, base+"/") {
						if !swiftExcludedPath(base, file, target.excludes) {
							m.files[file] = target.name
						} else {
							m.files[file] = ""
						}
					}
				}
				continue
			}
			for _, candidate := range target.paths {
				if target.sourcesKnown {
					candidate = pathpkg.Join(base, candidate)
				} else {
					candidate = base
				}
				if swiftExcludedPath(base, candidate, target.excludes) {
					continue
				}
				if old, ok := m.files[candidate]; ok && old != target.name {
					m.files[candidate] = ""
				} else {
					m.files[candidate] = target.name
				}
			}
		}
		m.manifests = true
	}
	m.fingerprint = hex.EncodeToString(h.Sum(nil))
	return m, nil
}

type parsedSwiftTarget struct {
	name, kind, path  string
	sources, excludes []string
	sourcesKnown      bool
	excludesKnown     bool
	paths             []string
}

func parseSwiftTargets(src string) []parsedSwiftTarget {
	var out []parsedSwiftTarget
	for _, match := range swiftTargetCall.FindAllStringIndex(src, -1) {
		start := match[1] - 1
		end := balancedSwiftCall(src, start)
		if end < 0 {
			continue
		}
		call := src[match[0]:match[1]]
		kind := strings.TrimSuffix(strings.TrimPrefix(call[:strings.Index(call, "(")], "."), "Target")
		body := src[start+1 : end]
		name, ok := swiftStringArgument(body, "name")
		if !ok {
			continue
		}
		t := parsedSwiftTarget{name: name, kind: strings.ToLower(kind), excludesKnown: true}
		if hasSwiftArgument(body, "path") {
			var pathOK bool
			t.path, pathOK = swiftStringArgument(body, "path")
			if !pathOK {
				continue
			}
		}
		if hasSwiftArgument(body, "sources") && !strings.Contains(body, "sources: [") {
			continue
		}
		if hasSwiftArgument(body, "exclude") {
			values, ok := swiftStringArrayArgument(body, "exclude")
			if !ok {
				t.excludesKnown = false
			} else {
				t.excludes = values
			}
		}
		if values, ok := swiftStringArrayArgument(body, "sources"); ok {
			t.sources, t.sourcesKnown, t.paths = values, true, values
		}
		out = append(out, t)
	}
	return out
}

func swiftExcludedPath(base, candidate string, excludes []string) bool {
	rel := candidate
	if base != "" && base != "." {
		if rel == base {
			rel = ""
		} else if strings.HasPrefix(rel, base+"/") {
			rel = strings.TrimPrefix(rel, base+"/")
		} else {
			return false
		}
	}
	for _, exclude := range excludes {
		exclude = strings.Trim(exclude, "/")
		if rel == exclude || strings.HasPrefix(rel, exclude+"/") {
			return true
		}
	}
	return false
}

func writeSwiftFingerprintField(h interface{ Write([]byte) (int, error) }, value string) {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(len(value)))
	h.Write(buf[:])
	h.Write([]byte(value))
}

func hasSwiftArgument(body, key string) bool {
	return regexp.MustCompile(`(?m)\b` + regexp.QuoteMeta(key) + `\s*:`).MatchString(body)
}

func balancedSwiftCall(src string, open int) int {
	depth := 0
	quote := byte(0)
	for i := open; i < len(src); i++ {
		c := src[i]
		if quote != 0 {
			if c == quote && (i == 0 || src[i-1] != '\\') {
				quote = 0
			}
			continue
		}
		if c == '"' {
			quote = c
			continue
		}
		if c == '(' {
			depth++
		}
		if c == ')' {
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func swiftStringArgument(body, key string) (string, bool) {
	re := regexp.MustCompile(`(?m)\b` + regexp.QuoteMeta(key) + `\s*:\s*"([^"]*)"`)
	m := re.FindStringSubmatch(body)
	if len(m) != 2 {
		return "", false
	}
	v, e := strconv.Unquote(`"` + m[1] + `"`)
	return v, e == nil
}
func swiftStringArrayArgument(body, key string) ([]string, bool) {
	re := regexp.MustCompile(`(?m)\b` + regexp.QuoteMeta(key) + `\s*:\s*\[([^]]*)\]`)
	m := re.FindStringSubmatch(body)
	if len(m) != 2 {
		return nil, false
	}
	vals := regexp.MustCompile(`"([^"]*)"`).FindAllStringSubmatch(m[1], -1)
	if len(vals) == 0 {
		return []string{}, strings.TrimSpace(m[1]) == ""
	}
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		out = append(out, v[1])
	}
	return out, true
}
