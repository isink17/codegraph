package gotool

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func BinPathHints() []string {
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

func VerifyCommandHint(binaryName string) string {
	if runtime.GOOS == "windows" {
		return "where.exe " + binaryName
	}
	return "command -v " + binaryName
}

func goEnv(name string) string {
	out, err := exec.Command("go", "env", name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
