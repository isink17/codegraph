package doctor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/isink17/codegraph/internal/appname"
	"github.com/isink17/codegraph/internal/config"
	"github.com/isink17/codegraph/internal/gotool"
	"github.com/isink17/codegraph/internal/platform"
	"github.com/isink17/codegraph/internal/store"
)

type Report struct {
	GOOS            string   `json:"goos"`
	ConfigPath      string   `json:"config_path"`
	ConfigExists    bool     `json:"config_exists"`
	DataDir         string   `json:"data_dir"`
	CacheDir        string   `json:"cache_dir"`
	CodegraphOnPath bool     `json:"codegraph_on_path"`
	CodegraphPath   string   `json:"codegraph_path,omitempty"`
	SQLiteDriver    string   `json:"sqlite_driver"`
	DB              *DBInfo  `json:"db,omitempty"`
	AppliedFixes    []string `json:"applied_fixes,omitempty"`
	Recommendations []string `json:"recommendations,omitempty"`
}

type DBInfo struct {
	Path      string          `json:"path"`
	SizeBytes int64           `json:"size_bytes"`
	Pragmas   store.DBPragmas `json:"pragmas"`
}

func Run() (Report, error) {
	return RunWithFix(false, "")
}

func RunWithFix(fix bool, dbPath string) (Report, error) {
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
		// Be conservative: doctor should still run even if the PATH lookup found something unusable.
		onPath = false
		binaryPath = ""
	}
	appliedFixes := []string{}
	if fix {
		defaultCfg, err := config.Default()
		if err != nil {
			return Report{}, err
		}
		for _, dir := range []string{paths.ConfigDir, paths.DataDir, paths.CacheDir, defaultCfg.DBDir} {
			if config.IsRepoDBDir(dir) {
				continue
			}
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

	var dbInfo *DBInfo
	if strings.TrimSpace(dbPath) != "" {
		info, err := inspectDB(context.Background(), dbPath)
		if err != nil {
			recommendations = append(recommendations, "repo DB inspect failed: "+err.Error())
		} else {
			dbInfo = info
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
		SQLiteDriver:    store.SQLiteDriverName(),
		DB:              dbInfo,
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

func inspectDB(ctx context.Context, dbPath string) (*DBInfo, error) {
	st, err := os.Stat(dbPath)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open(store.SQLiteDriverName(), dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	pragmas, err := store.QueryDBPragmas(ctx, db)
	if err != nil {
		return nil, err
	}

	return &DBInfo{
		Path:      filepath.Clean(dbPath),
		SizeBytes: st.Size(),
		Pragmas:   pragmas,
	}, nil
}
