package doctor

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/isink17/codegraph/internal/appname"
	"github.com/isink17/codegraph/internal/config"
	"github.com/isink17/codegraph/internal/gotool"
	"github.com/isink17/codegraph/internal/platform"
)

type Report struct {
	GOOS            string   `json:"goos"`
	ConfigPath      string   `json:"config_path"`
	ConfigExists    bool     `json:"config_exists"`
	DataDir         string   `json:"data_dir"`
	CacheDir        string   `json:"cache_dir"`
	CodegraphOnPath bool     `json:"codegraph_on_path"`
	CodegraphPath   string   `json:"codegraph_path,omitempty"`
	AppliedFixes    []string `json:"applied_fixes,omitempty"`
	Recommendations []string `json:"recommendations,omitempty"`
}

func Run() (Report, error) {
	return RunWithFix(false)
}

func RunWithFix(fix bool) (Report, error) {
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
	appliedFixes := []string{}
	if fix {
		defaultCfg, err := config.Default()
		if err != nil {
			return Report{}, err
		}
		for _, dir := range []string{paths.ConfigDir, paths.DataDir, paths.CacheDir, defaultCfg.DBDir} {
			if err := os.MkdirAll(dir, 0o755); err == nil {
				appliedFixes = append(appliedFixes, "ensured directory: "+dir)
			}
		}
		if _, created, err := config.SaveIfMissing(defaultCfg); err == nil && created {
			appliedFixes = append(appliedFixes, "created default config: "+configPath)
			exists = true
		}
	}
	recommendations := []string{}
	if !onPath {
		recommendations = append(recommendations, "codegraph binary was not found on PATH")
		for _, hint := range gotool.BinPathHints() {
			recommendations = append(recommendations, "add to PATH: "+hint)
		}
		if hint := firstPathHint(); hint != "" {
			if runtime.GOOS == "windows" {
				recommendations = append(recommendations, fmt.Sprintf(`temporary PowerShell PATH: $env:Path += ";%s"`, hint))
			} else {
				recommendations = append(recommendations, fmt.Sprintf(`temporary PATH: export PATH="%s:$PATH"`, hint))
			}
		}
		recommendations = append(recommendations, "verify after reopening shell: "+gotool.VerifyCommandHint(appname.BinaryName))
	}

	return Report{
		GOOS:            runtime.GOOS,
		ConfigPath:      filepath.Clean(configPath),
		ConfigExists:    exists,
		DataDir:         paths.DataDir,
		CacheDir:        paths.CacheDir,
		CodegraphOnPath: onPath,
		CodegraphPath:   binaryPath,
		AppliedFixes:    appliedFixes,
		Recommendations: recommendations,
	}, nil
}

func firstPathHint() string {
	hints := gotool.BinPathHints()
	if len(hints) == 0 {
		return ""
	}
	return hints[0]
}
