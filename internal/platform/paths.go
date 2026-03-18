package platform

import (
	"os"
	"path/filepath"
	"runtime"

	"github.com/example/localcodegraph/internal/appname"
)

type Paths struct {
	ConfigDir string
	DataDir   string
	CacheDir  string
}

func DefaultPaths() (Paths, error) {
	if override := os.Getenv("CODEGRAPH_HOME"); override != "" {
		base := filepath.Clean(override)
		return Paths{
			ConfigDir: filepath.Join(base, "config"),
			DataDir:   filepath.Join(base, "data"),
			CacheDir:  filepath.Join(base, "cache"),
		}, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, err
	}

	switch runtime.GOOS {
	case "darwin":
		base := filepath.Join(home, "Library", "Application Support", appname.ConfigDirName)
		return Paths{
			ConfigDir: base,
			DataDir:   base,
			CacheDir:  filepath.Join(home, "Library", "Caches", appname.ConfigDirName),
		}, nil
	case "windows":
		appData := os.Getenv("AppData")
		localAppData := os.Getenv("LocalAppData")
		if appData == "" {
			appData = filepath.Join(home, "AppData", "Roaming")
		}
		if localAppData == "" {
			localAppData = filepath.Join(home, "AppData", "Local")
		}
		return Paths{
			ConfigDir: filepath.Join(appData, appname.ConfigDirName),
			DataDir:   filepath.Join(localAppData, appname.ConfigDirName),
			CacheDir:  filepath.Join(localAppData, appname.ConfigDirName, "cache"),
		}, nil
	default:
		configHome := os.Getenv("XDG_CONFIG_HOME")
		if configHome == "" {
			configHome = filepath.Join(home, ".config")
		}
		dataHome := os.Getenv("XDG_DATA_HOME")
		if dataHome == "" {
			dataHome = filepath.Join(home, ".local", "share")
		}
		cacheHome := os.Getenv("XDG_CACHE_HOME")
		if cacheHome == "" {
			cacheHome = filepath.Join(home, ".cache")
		}
		return Paths{
			ConfigDir: filepath.Join(configHome, appname.ConfigDirName),
			DataDir:   filepath.Join(dataHome, appname.ConfigDirName),
			CacheDir:  filepath.Join(cacheHome, appname.ConfigDirName),
		}, nil
	}
}
