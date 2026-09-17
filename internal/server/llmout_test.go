package server

import "testing"

func TestCleanLLMText(t *testing.T) {
	if got := cleanLLMText("  hello  ", 100); got != "hello" {
		t.Errorf("cleanLLMText trim = %q", got)
	}
	got := cleanLLMText("abcdefghij", 5)
	if got != "abcde…" {
		t.Errorf("cleanLLMText cap = %q, want abcde…", got)
	}
}

func TestValidLLMBool(t *testing.T) {
	cases := map[any]bool{
		true:       true,
		false:      false,
		"true":     true,
		"yes":      true,
		"1":        true,
		"on":       true,
		"false":    false,
		"0":        false,
		"nope":     false,
		float64(1): true,
		float64(0): false,
		nil:        false,
	}
	for in, want := range cases {
		if got := validLLMBool(in); got != want {
			t.Errorf("validLLMBool(%v) = %v, want %v", in, got, want)
		}
	}
}

func TestCapLLMList(t *testing.T) {
	in := []int{1, 2, 3, 4, 5}
	if got := capLLMList(in, 3); len(got) != 3 || got[0] != 1 {
		t.Errorf("capLLMList(5,3) = %v", got)
	}
	if got := capLLMList(in, 10); len(got) != 5 {
		t.Errorf("capLLMList(5,10) = %v", got)
	}
}
