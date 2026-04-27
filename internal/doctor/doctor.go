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
	GOOS            string    `json:"goos"`
	ConfigPath      string    `json:"config_path"`
	ConfigExists    bool      `json:"config_exists"`
	DataDir         string    `json:"data_dir"`
	CacheDir        string    `json:"cache_dir"`
	CodegraphOnPath bool      `json:"codegraph_on_path"`
	CodegraphPath   string    `json:"codegraph_path,omitempty"`
	SQLiteDriver    string    `json:"sqlite_driver"`
	DB              *DBInfo   `json:"db,omitempty"`
	Deep            *DeepInfo `json:"deep,omitempty"`
	AppliedFixes    []string  `json:"applied_fixes"`
	Recommendations []string  `json:"recommendations"`
}

type DBInfo struct {
	Path      string          `json:"path"`
	SizeBytes int64           `json:"size_bytes"`
	Pragmas   store.DBPragmas `json:"pragmas"`
}

type DeepInfo struct {
	DB *DBDeepInfo `json:"db,omitempty"`
}

type DBDeepInfo struct {
	IntegrityOK        bool     `json:"integrity_ok"`
	IntegrityMessages  []string `json:"integrity_messages,omitempty"`
	IntegrityTruncated bool     `json:"integrity_truncated,omitempty"`
	ForeignKeyIssues   int64    `json:"foreign_key_issues,omitempty"`
}

func Run() (Report, error) {
	return RunWithFix(false, "")
}

func RunWithFix(fix bool, dbPath string) (Report, error) {
	return RunWithOptions(Options{Fix: fix, DBPath: dbPath})
}

type Options struct {
	Fix    bool
	DBPath string
	Deep   bool
}

func RunWithOptions(opts Options) (Report, error) {
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
	if opts.Fix {
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
	var deepInfo *DeepInfo
	if strings.TrimSpace(opts.DBPath) != "" {
		info, err := inspectDB(context.Background(), opts.DBPath)
		if err != nil {
			recommendations = append(recommendations, "repo DB inspect failed: "+err.Error())
		} else {
			dbInfo = info
		}
		if opts.Deep {
			di, err := inspectDeepDB(context.Background(), opts.DBPath)
			if err != nil {
				recommendations = append(recommendations, "repo DB deep inspect failed: "+err.Error())
			} else {
				deepInfo = &DeepInfo{DB: di}
			}
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
		Deep:            deepInfo,
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
	dsn, err := store.BuildSQLiteDSN(dbPath, store.OpenOptions{}, false, true)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open(store.SQLiteDriverName(), dsn)
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

func inspectDeepDB(ctx context.Context, dbPath string) (*DBDeepInfo, error) {
	dsn, err := store.BuildSQLiteDSN(dbPath, store.OpenOptions{}, false, true)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open(store.SQLiteDriverName(), dsn)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	// integrity_check can return many rows; keep this bounded and human-usable.
	const maxIntegrityMessages = 20
	integrityMessages := make([]string, 0)
	truncated := false
	rows, err := db.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var msg string
		if err := rows.Scan(&msg); err != nil {
			rows.Close()
			return nil, err
		}
		if len(integrityMessages) < maxIntegrityMessages {
			integrityMessages = append(integrityMessages, msg)
		} else {
			truncated = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	integrityOK := len(integrityMessages) == 1 && integrityMessages[0] == "ok"
	if integrityOK {
		integrityMessages = nil
		truncated = false
	}

	var foreignKeyIssues int64
	fkRows, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return nil, err
	}
	for fkRows.Next() {
		foreignKeyIssues++
	}
	if err := fkRows.Err(); err != nil {
		fkRows.Close()
		return nil, err
	}
	fkRows.Close()

	return &DBDeepInfo{
		IntegrityOK:        integrityOK,
		IntegrityMessages:  integrityMessages,
		IntegrityTruncated: truncated,
		ForeignKeyIssues:   foreignKeyIssues,
	}, nil
}
