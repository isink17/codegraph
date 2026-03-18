package doctor

import (
	"errors"
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
		for _, hint := range gotool.BinPathHints() {
			recommendations = append(recommendations, "add to PATH: "+hint)
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
		Recommendations: recommendations,
	}, nil
}
