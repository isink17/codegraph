package limits

import "testing"

// TestPageRowsNormalizes pins the store backstop's contract: 0 and negative
// select the default, ordinary values pass through untouched, and no value
// however large reaches a query as itself.
func TestPageRowsNormalizes(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"zero means default", 0, DefaultPage},
		{"negative means default", -5, DefaultPage},
		{"one is honoured", 1, 1},
		{"default is honoured", DefaultPage, DefaultPage},
		{"public maximum is honoured", MaxPage, MaxPage},
		{"internal over-fetch is honoured", MaxPage * 3, MaxPage * 3},
		{"backstop is honoured", StoreMaxRows, StoreMaxRows},
		{"above the backstop is clamped", StoreMaxRows + 1, StoreMaxRows},
		{"absurd is clamped", 100_000_000, StoreMaxRows},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PageRows(tc.in); got != tc.want {
				t.Fatalf("PageRows(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestStoreBackstopClearsPublicMaximum is the reason StoreMaxRows is not simply
// MaxPage: hybrid search asks the store for three times the requested window
// before fusing, and the backstop must not silently trim a request the public
// policy already accepted.
func TestStoreBackstopClearsPublicMaximum(t *testing.T) {
	overFetch := MaxPage * 3
	if overFetch > StoreMaxRows {
		t.Fatalf("StoreMaxRows = %d, too small for the %d-row internal over-fetch", StoreMaxRows, overFetch)
	}
	if StoreMaxRows <= MaxPage {
		t.Fatalf("StoreMaxRows = %d must exceed MaxPage = %d", StoreMaxRows, MaxPage)
	}
}

func TestExportRowsNormalizes(t *testing.T) {
	cases := []struct{ in, want int }{
		{0, DefaultExportPage},
		{-1, DefaultExportPage},
		{1, 1},
		{MaxPage, MaxPage},
		{MaxExportPage, MaxExportPage},
		{MaxExportPage + 1, MaxExportPage},
		{100_000_000, MaxExportPage},
	}
	for _, tc := range cases {
		if got := ExportRows(tc.in); got != tc.want {
			t.Fatalf("ExportRows(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
	// Export pages with a bigger window than a query does, so it cannot be
	// forced through the query ceiling.
	if ExportRows(MaxPage*2) != MaxPage*2 {
		t.Fatalf("ExportRows must not clamp a legitimate export page to the query ceiling")
	}
}

// TestOffsetIsFlooredNotCapped records the measured decision: a large offset
// costs no more than scanning the rows that exist, so only the negative side
// needs normalizing.
func TestOffsetIsFlooredNotCapped(t *testing.T) {
	if got := Offset(-1); got != 0 {
		t.Fatalf("Offset(-1) = %d, want 0", got)
	}
	for _, in := range []int{0, 1, 1000, 100_000_000} {
		if got := Offset(in); got != in {
			t.Fatalf("Offset(%d) = %d, want it unchanged", in, got)
		}
	}
}

// TestMaxPageForDetailScalesDownWithSourceCost pins the shape of the ladder
// rather than the exact numbers: a level that carries more bytes per row must
// not be allowed more rows than a cheaper one.
func TestMaxPageForDetailScalesDownWithSourceCost(t *testing.T) {
	card := MaxPageForDetail("card")
	skeleton := MaxPageForDetail("skeleton")
	excerpt := MaxPageForDetail("excerpt")
	full := MaxPageForDetail("full")

	if card != MaxPage {
		t.Fatalf("card ceiling = %d, want MaxPage = %d", card, MaxPage)
	}
	if !(card > skeleton && skeleton > excerpt) {
		t.Fatalf("ceilings must fall as row cost rises: card=%d skeleton=%d excerpt=%d", card, skeleton, excerpt)
	}
	if excerpt != full {
		t.Fatalf("excerpt=%d and full=%d both carry source and should share a ceiling", excerpt, full)
	}
	// An unset or unknown level is the default, which is card.
	if MaxPageForDetail("") != card {
		t.Fatalf("an omitted detail level must use the card ceiling")
	}
}
