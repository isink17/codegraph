//go:build fixturebroken

package a

func TestBroken(t *testing.T) { undefined() }
