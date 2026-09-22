package config

import "testing"

// TestParseIDList pins the collection-scope flag parsing: comma-separated
// positive ints survive, empty/zero/non-numeric segments are dropped.
func TestParseIDList(t *testing.T) {
	if got := parseIDList(""); len(got) != 0 {
		t.Fatalf("empty -> %v, want none", got)
	}
	if got := parseIDList("3,7"); len(got) != 2 || got[0] != 3 || got[1] != 7 {
		t.Fatalf("3,7 -> %v", got)
	}
	if got := parseIDList("0,-1,abc"); len(got) != 0 {
		t.Fatalf("junk -> %v, want none", got)
	}
	if got := parseIDList(" 2 , 4 "); len(got) != 2 || got[0] != 2 || got[1] != 4 {
		t.Fatalf("spaces -> %v", got)
	}
}
