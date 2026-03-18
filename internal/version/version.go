package version

import (
	"runtime/debug"
	"strings"
)

// Version can be overridden at build time:
// go build -ldflags "-X github.com/isink17/codegraph/internal/version.Version=v0.1.0"
var Version = "dev"

func Current() string {
	if v := strings.TrimSpace(Version); v != "" && v != "dev" {
		return v
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	v := strings.TrimSpace(info.Main.Version)
	if v == "" || v == "(devel)" {
		return "dev"
	}
	return v
}
