package addr

import "testing"

func TestParse(t *testing.T) {
	good := []string{"0", "0.1", "0.1.2"}
	for _, s := range good {
		if _, err := Parse(s); err != nil {
			t.Errorf("Parse(%q) unexpected error: %v", s, err)
		}
	}
	bad := []string{"", "1", "0.", ".0", "0..1", "x"}
	for _, s := range bad {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) expected error, got nil", s)
		}
	}
}

func TestChildParent(t *testing.T) {
	a := Root.Child("1").Child("2")
	if a.String() != "0.1.2" {
		t.Fatalf("got %q want 0.1.2", a)
	}
	p, ok := a.Parent()
	if !ok || p != Address("0.1") {
		t.Fatalf("Parent() = %q,%v want 0.1,true", p, ok)
	}
	if _, ok := Root.Parent(); ok {
		t.Fatalf("Root.Parent() ok = true, want false")
	}
}

func TestIsRoot(t *testing.T) {
	if !Root.IsRoot() {
		t.Fatal("Root.IsRoot() = false")
	}
	if Root.Child("1").IsRoot() {
		t.Fatal("child IsRoot() = true")
	}
}

func TestIsAncestorOf(t *testing.T) {
	cases := []struct {
		a, d Address
		want bool
	}{
		{"0.1", "0.1.2", true},
		{"0.1", "0.1.2.3", true},
		{"0", "0.1", true},
		{"0.1", "0.1", false},   // not its own ancestor
		{"0.1", "0.10", false},  // prefix trap
		{"0.1", "0.2", false},   // sibling
		{"0.1.2", "0.1", false}, // reversed
	}
	for _, c := range cases {
		if got := c.a.IsAncestorOf(c.d); got != c.want {
			t.Errorf("%s.IsAncestorOf(%s) = %v want %v", c.a, c.d, got, c.want)
		}
	}
}
