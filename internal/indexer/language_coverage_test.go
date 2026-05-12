package indexer

import (
	"testing"

	"github.com/isink17/codegraph/internal/store"
)

func TestUpdateLanguageCoverage_UnknownExtensions(t *testing.T) {
	t.Parallel()

	cov := map[string]store.LanguageCounts{}

	updateLanguageCoverage(cov, "unknown", "Assets/IMAGE.TIF", store.LanguageCounts{Seen: 1, Skipped: 1})
	updateLanguageCoverage(cov, "unknown", "Assets/logo.png", store.LanguageCounts{Seen: 1})
	updateLanguageCoverage(cov, "unknown", "Assets/README", store.LanguageCounts{Seen: 1, Indexed: 1})
	updateLanguageCoverage(cov, "unknown", "Assets/README", store.LanguageCounts{ParseFailed: 1, Skipped: 1})

	got := cov["unknown"]
	if got.Seen != 3 {
		t.Fatalf("unknown.seen=%d, want 3", got.Seen)
	}
	if got.Indexed != 1 {
		t.Fatalf("unknown.indexed=%d, want 1", got.Indexed)
	}
	if got.Skipped != 2 {
		t.Fatalf("unknown.skipped=%d, want 2", got.Skipped)
	}
	if got.ParseFailed != 1 {
		t.Fatalf("unknown.parse_failed=%d, want 1", got.ParseFailed)
	}

	if got.Extensions == nil {
		t.Fatalf("unknown.extensions is nil")
	}
	if got.Extensions[".tif"].Seen != 1 || got.Extensions[".tif"].Skipped != 1 {
		t.Fatalf("unknown.extensions[.tif]=%+v, want seen=1 skipped=1", got.Extensions[".tif"])
	}
	if got.Extensions[".png"].Seen != 1 {
		t.Fatalf("unknown.extensions[.png]=%+v, want seen=1", got.Extensions[".png"])
	}
	noExt := got.Extensions["[no extension]"]
	if noExt.Seen != 1 || noExt.Indexed != 1 || noExt.Skipped != 1 || noExt.ParseFailed != 1 {
		t.Fatalf("unknown.extensions[[no extension]]=%+v, want seen=1 indexed=1 skipped=1 parse_failed=1", noExt)
	}
}

func TestUpdateLanguageCoverage_KnownLanguageUnchanged(t *testing.T) {
	t.Parallel()

	cov := map[string]store.LanguageCounts{}

	updateLanguageCoverage(cov, "go", "main.go", store.LanguageCounts{Seen: 1, Indexed: 1})

	got := cov["go"]
	if got.Seen != 1 || got.Indexed != 1 || got.Skipped != 0 || got.ParseFailed != 0 {
		t.Fatalf("go counts=%+v, want seen=1 indexed=1", got)
	}
	if got.Extensions != nil {
		t.Fatalf("go.extensions=%v, want nil", got.Extensions)
	}
}

