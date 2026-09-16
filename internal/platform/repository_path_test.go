package platform

import "testing"

func TestLogicalRepositoryPath(t *testing.T) {
	for _, p := range []string{"a/b.go", "weird\\name.go", "weird name/žaba.go"} {
		if got, err := LogicalRepositoryPath(p); err != nil || got != p {
			t.Fatalf("%q: %q %v", p, got, err)
		}
	}
	for _, p := range []string{"", ".", "..", "a/../b", "a//b", "a/", "/a", `C:\\a`, "C:/a", `\\server\\share\\a`} {
		if _, err := LogicalRepositoryPath(p); err == nil {
			t.Fatalf("accepted %q", p)
		}
	}
}
