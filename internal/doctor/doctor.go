package doctor

import (
	"errors"
	"os/exec"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/isink17/codegraph/internal/config"
	"github.com/isink17/codegraph/internal/platform"
)

type Report struct {
	GOOS            string `json:"goos"`
	ConfigPath      string `json:"config_path"`
	ConfigExists    bool   `json:"config_exists"`
	DataDir         string `json:"data_dir"`
	CacheDir        string `json:"cache_dir"`
	CodegraphOnPath bool   `json:"codegraph_on_path"`
	CodegraphPath   string `json:"codegraph_path,omitempty"`
	Recommendations []string `json:"recommendations,omitempty"`
}

func Run() (Report, error) {
	paths, err := platform.DefaultPaths()
	if err != nil {
		return Report{}, err
	}
	configPath, err := config.ConfigPath()
	if err != nil {
		return Report{}, err
	}
	_, err = os.Stat(configPath)
	exists := err == nil
	binaryPath, lookErr := exec.LookPath("codegraph")
	onPath := lookErr == nil
	if lookErr != nil && !errors.Is(lookErr, exec.ErrNotFound) {
		return Report{}, lookErr
	}
	recommendations := []string{}
	if !onPath {
		recommendations = append(recommendations, "codegraph binary was not found on PATH")
		for _, hint := range goBinPathHints() {
			recommendations = append(recommendations, "add to PATH: "+hint)
		}
		if runtime.GOOS == "windows" {
			recommendations = append(recommendations, "verify after reopening PowerShell: where.exe codegraph")
		} else {
			recommendations = append(recommendations, "verify after reopening shell: command -v codegraph")
		}
	}

	return Report{
		GOOS:            runtime.GOOS,
		ConfigPath:      filepath.Clean(configPath),
		ConfigExists:    exists,
		DataDir:         paths.DataDir,
		CacheDir:        paths.CacheDir,
		CodegraphOnPath: onPath,
		CodegraphPath:   binaryPath,
		Recommendations: recommendations,
	}, nil
}

func goBinPathHints() []string {
	var hints []string
	if gobin := strings.TrimSpace(goEnv("GOBIN")); gobin != "" {
		hints = append(hints, gobin)
	}
	if gopath := strings.TrimSpace(goEnv("GOPATH")); gopath != "" {
		hints = append(hints, filepath.Join(gopath, "bin"))
	}
	home, err := os.UserHomeDir()
	if err == nil {
		hints = append(hints, filepath.Join(home, "go", "bin"))
	}
	if runtime.GOOS == "windows" {
		hints = append(hints, `C:\Program Files\Go\bin`)
	} else {
		hints = append(hints, "/usr/local/go/bin")
	}
	seen := map[string]struct{}{}
	var uniq []string
	for _, h := range hints {
		if h == "" {
			continue
		}
		if _, ok := seen[h]; ok {
			continue
		}
		seen[h] = struct{}{}
		uniq = append(uniq, h)
	}
	return uniq
}

func goEnv(name string) string {
	out, err := exec.Command("go", "env", name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
