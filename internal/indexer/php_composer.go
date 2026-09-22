package indexer

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/isink17/codegraph/internal/platform"
)

const phpComposerManifestPath = "composer.json"

type phpComposerPSR4State string

const (
	phpComposerPSR4Absent   phpComposerPSR4State = "absent"
	phpComposerPSR4Valid    phpComposerPSR4State = "valid"
	phpComposerPSR4Disabled phpComposerPSR4State = "disabled"
)

type phpComposerPSR4Mapping struct {
	ManifestPath    string
	MappingRole     string
	NamespacePrefix string
	RootPath        string
	RootOrdinal     int
}

type phpComposerPSR4Discovery struct {
	Mappings    []phpComposerPSR4Mapping
	Fingerprint string
	State       phpComposerPSR4State
}

// discoverPHPComposerPSR4 reads only root/composer.json. Invalid Composer
// metadata disables this evidence bridge without making repository indexing fail.
func discoverPHPComposerPSR4(root string) (phpComposerPSR4Discovery, error) {
	manifest := filepath.Join(root, phpComposerManifestPath)
	raw, err := os.ReadFile(manifest)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return phpComposerDiscovery(phpComposerPSR4Absent, nil, nil), nil
		}
		return phpComposerPSR4Discovery{}, err
	}
	mappings, err := parsePHPComposerPSR4(root, raw)
	if err != nil {
		return phpComposerDiscovery(phpComposerPSR4Disabled, raw, nil), nil
	}
	return phpComposerDiscovery(phpComposerPSR4Valid, raw, mappings), nil
}

func phpComposerDiscovery(state phpComposerPSR4State, raw []byte, mappings []phpComposerPSR4Mapping) phpComposerPSR4Discovery {
	h := sha256.New()
	h.Write([]byte("php-composer-psr4-v1\x00"))
	writePHPComposerFingerprintField(h, string(state))
	if state != phpComposerPSR4Absent {
		writePHPComposerFingerprintField(h, phpComposerManifestPath)
		writePHPComposerFingerprintField(h, string(raw))
	}
	return phpComposerPSR4Discovery{Mappings: mappings, Fingerprint: hex.EncodeToString(h.Sum(nil)), State: state}
}

func writePHPComposerFingerprintField(h interface{ Write([]byte) (int, error) }, value string) {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(len(value)))
	h.Write(buf[:])
	h.Write([]byte(value))
}

func parsePHPComposerPSR4(root string, raw []byte) ([]phpComposerPSR4Mapping, error) {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, err
	}
	if document == nil {
		return nil, errors.New("invalid Composer manifest")
	}
	var mappings []phpComposerPSR4Mapping
	for _, section := range []struct{ name, role string }{{"autoload", "autoload"}, {"autoload-dev", "autoload-dev"}} {
		sectionRaw, ok := document[section.name]
		if !ok {
			continue
		}
		var autoload map[string]json.RawMessage
		if err := json.Unmarshal(sectionRaw, &autoload); err != nil {
			return nil, err
		}
		if autoload == nil {
			return nil, errors.New("invalid Composer autoload section")
		}
		psr4Raw, ok := autoload["psr-4"]
		if !ok {
			continue
		}
		var prefixes map[string]json.RawMessage
		if err := json.Unmarshal(psr4Raw, &prefixes); err != nil {
			return nil, err
		}
		if prefixes == nil {
			return nil, errors.New("invalid Composer PSR-4 section")
		}
		keys := make([]string, 0, len(prefixes))
		for prefix := range prefixes {
			keys = append(keys, prefix)
		}
		sort.Strings(keys)
		for _, prefix := range keys {
			if prefix == "" || !strings.HasSuffix(prefix, `\`) {
				return nil, errors.New("invalid Composer PSR-4 namespace prefix")
			}
			roots, err := phpComposerRoots(root, prefixes[prefix])
			if err != nil {
				return nil, err
			}
			for ordinal, rootPath := range roots {
				mappings = append(mappings, phpComposerPSR4Mapping{
					ManifestPath: phpComposerManifestPath, MappingRole: section.role, NamespacePrefix: prefix, RootPath: rootPath, RootOrdinal: ordinal,
				})
			}
		}
	}
	return mappings, nil
}

func phpComposerRoots(repoRoot string, raw json.RawMessage) ([]string, error) {
	var scalar string
	if err := json.Unmarshal(raw, &scalar); err == nil {
		root, err := phpComposerRootPath(repoRoot, scalar)
		return []string{root}, err
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, errors.New("invalid Composer PSR-4 root")
	}
	roots := make([]string, len(values))
	for i, value := range values {
		root, err := phpComposerRootPath(repoRoot, value)
		if err != nil {
			return nil, err
		}
		roots[i] = root
	}
	return roots, nil
}

func phpComposerRootPath(repoRoot, raw string) (string, error) {
	if raw == "." || raw == "./" {
		return ".", nil
	}
	if raw == "" {
		return "", errors.New("empty Composer PSR-4 root")
	}
	native := filepath.FromSlash(raw)
	if filepath.IsAbs(native) || filepath.VolumeName(native) != "" {
		return "", ErrPathOutsideRepo
	}
	for _, part := range strings.Split(native, string(filepath.Separator)) {
		if part == ".." {
			return "", ErrPathOutsideRepo
		}
	}
	logical, err := platform.PublicRepositoryPath(raw)
	if err != nil {
		return "", ErrPathOutsideRepo
	}
	root := absClean(repoRoot)
	target := filepath.Join(root, native)
	if !pathWithin(root, target) {
		return "", ErrPathOutsideRepo
	}
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == "." {
		return "", ErrPathOutsideRepo
	}
	canonical, err := platform.NativeRelativeToLogical(rel)
	if err != nil || canonical != logical {
		return "", ErrPathOutsideRepo
	}
	return canonical, nil
}
