package watcher

import "testing"

func TestWatcherConfigPathUsesLogicalSeparators(t *testing.T) {
	if !isWatcherConfigPath(".codegraph/config.json") {
		t.Fatal("slash config path not recognized")
	}
	if isWatcherConfigPath(`.codegraph\config.json`) {
		t.Fatal("literal backslash config path recognized")
	}
}

func TestRelPathWithinRepoPreservesLogicalBackslash(t *testing.T) {
	if !isRelPathWithinRepo(`dir\file.go`) {
		t.Fatal("literal backslash logical path rejected")
	}
	if isRelPathWithinRepo("../file.go") || isRelPathWithinRepo("..") {
		t.Fatal("traversal path accepted")
	}
}
