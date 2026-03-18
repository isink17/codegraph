package doctor

import (
	"os"
	"path/filepath"
	"runtime"

	"github.com/isink17/codegraph/internal/config"
	"github.com/isink17/codegraph/internal/platform"
)

type Report struct {
	GOOS         string `json:"goos"`
	ConfigPath   string `json:"config_path"`
	ConfigExists bool   `json:"config_exists"`
	DataDir      string `json:"data_dir"`
	CacheDir     string `json:"cache_dir"`
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
	return Report{
		GOOS:         runtime.GOOS,
		ConfigPath:   filepath.Clean(configPath),
		ConfigExists: exists,
		DataDir:      paths.DataDir,
		CacheDir:     paths.CacheDir,
	}, nil
}
