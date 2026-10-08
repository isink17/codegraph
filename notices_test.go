package codegraph

import (
	"os"
	"strings"
	"testing"
)

// The binary embeds the root files themselves, so there is no second copy to
// drift; this pins that the embedded text is exactly what is on disk.
func TestEmbeddedNoticesMatchRootFiles(t *testing.T) {
	for name, embedded := range map[string]string{"LICENSE": License, "THIRD_PARTY_NOTICES": ThirdPartyNotices} {
		disk, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if embedded == "" || string(disk) != embedded {
			t.Errorf("embedded %s differs from the file on disk", name)
		}
		// .gitattributes pins LF, so every platform's binary prints the
		// same bytes even from a Windows checkout with autocrlf.
		if strings.Contains(embedded, "\r") {
			t.Errorf("embedded %s contains CR line endings", name)
		}
	}
}
