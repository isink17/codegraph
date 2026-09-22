package indexer

import (
	"os"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type swiftTarget struct {
	name  string
	root  string
	paths []string
}

type swiftModuleMap struct {
	files     map[string]string
	packages  map[string]string
	manifests bool
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
		if !d.IsDir() && d.Name() == "Package.swift" {
			manifests = append(manifests, path)
		}
		return nil
	})
	if err != nil {
		return m, err
	}
	sort.Strings(manifests)
	for _, manifest := range manifests {
		data, err := os.ReadFile(manifest)
		if err != nil {
			return m, err
		}
		pkgRoot, err := filepath.Rel(root, filepath.Dir(manifest))
		if err != nil {
			return m, err
		}
		pkgRoot = strings.ReplaceAll(pkgRoot, "\\", "/")
		if pkgRoot == "." {
			pkgRoot = ""
		}
		packageScope := pkgRoot
		if packageScope == "" {
			packageScope = "."
		}
		for _, target := range parseSwiftTargets(string(data)) {
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
			if target.sourcesKnown {
				target.paths = target.sources
			}
			for _, candidate := range target.paths {
				if target.sourcesKnown {
					candidate = pathpkg.Join(base, candidate)
				} else {
					candidate = base
				}
				if old, ok := m.files[candidate]; ok && old != target.name {
					m.files[candidate] = ""
				} else {
					m.files[candidate] = target.name
				}
			}
			if !target.sourcesKnown {
				m.files[base+"/"] = target.name
			}
		}
		m.manifests = true
	}
	return m, nil
}

type parsedSwiftTarget struct {
	name, kind, path  string
	sources, excludes []string
	sourcesKnown      bool
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
		t := parsedSwiftTarget{name: name, kind: strings.ToLower(kind)}
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
		if values, ok := swiftStringArrayArgument(body, "sources"); ok {
			t.sources, t.sourcesKnown, t.paths = values, true, values
		}
		out = append(out, t)
	}
	return out
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
		return nil, false
	}
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		out = append(out, v[1])
	}
	return out, true
}
