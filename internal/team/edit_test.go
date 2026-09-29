package team

import "testing"

func TestReplaceOnce(t *testing.T) {
	got, err := replaceOnce("a := 1\nb := 2\n", "b := 2", "b := 3")
	if err != nil || got != "a := 1\nb := 3\n" {
		t.Fatalf("single match: %q %v", got, err)
	}
	if _, err := replaceOnce("x\nx\n", "x", "y"); err == nil {
		t.Fatal("accepted an ambiguous match")
	}
	if _, err := replaceOnce("x\n", "z", "y"); err == nil {
		t.Fatal("accepted a missing match")
	}
}
