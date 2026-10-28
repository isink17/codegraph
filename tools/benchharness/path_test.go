package main

import "testing"

func TestDriveRelativePath(t *testing.T) {
	if !isAbsolutePath(`C:fixture`) {
		t.Fatal("drive-relative fixture path accepted")
	}
}
