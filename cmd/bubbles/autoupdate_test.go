package main

import (
	"os"
	"testing"
)

func TestAutoUpdateEnabledOptOut(t *testing.T) {
	cases := map[string]bool{
		"":     true,  // default on
		"1":    true,  // only 0/off disable
		"on":   true,
		"0":    false,
		"off":  false,
		"OFF":  false,
		" off": false, // trimmed
	}
	for v, want := range cases {
		t.Setenv("BUBBLES_AUTO_UPDATE", v)
		if got := autoUpdateEnabled(); got != want {
			t.Errorf("BUBBLES_AUTO_UPDATE=%q -> enabled=%v, want %v", v, got, want)
		}
	}
	os.Unsetenv("BUBBLES_AUTO_UPDATE")
}
