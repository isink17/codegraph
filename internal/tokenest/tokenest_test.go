package tokenest

import "testing"

func TestFromBytesRoundsUp(t *testing.T) {
	cases := []struct{ in, want int }{
		{-1, 0}, {0, 0}, {1, 1}, {3, 1}, {4, 1}, {5, 2}, {8, 2}, {4001, 1001},
	}
	for _, c := range cases {
		if got := FromBytes(c.in); got != c.want {
			t.Fatalf("FromBytes(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestOfJSONMatchesMarshalledSize(t *testing.T) {
	v := map[string]any{"a": 1, "b": "two"}
	tokens, bytes, err := OfJSON(v)
	if err != nil {
		t.Fatalf("OfJSON() error = %v", err)
	}
	if bytes != len(`{"a":1,"b":"two"}`) {
		t.Fatalf("bytes = %d, want %d", bytes, len(`{"a":1,"b":"two"}`))
	}
	if tokens != FromBytes(bytes) {
		t.Fatalf("tokens = %d, want %d", tokens, FromBytes(bytes))
	}
}

func TestOfStringEqualsFromBytes(t *testing.T) {
	if OfString("abcde") != FromBytes(5) {
		t.Fatal("OfString and FromBytes disagree")
	}
}
