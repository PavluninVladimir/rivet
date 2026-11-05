package main

import (
	"strings"
	"testing"
)

// Runner сам объявляет capability architecture, кроме runner'а выкатки
// (add-architecture-c4).
func TestWithArchitectureCapability(t *testing.T) {
	cases := []struct{ in, want string }{
		{"coding", "coding,architecture"},
		{"coding,review", "coding,review,architecture"},
		{" coding , ", "coding,architecture"},
		{"deploy", "deploy"},
		{"deploy,coding", "deploy,coding,architecture"},
		{"coding,architecture", "coding,architecture"},
		{"", ""},
	}
	for _, c := range cases {
		got := strings.Join(withArchitectureCapability(strings.Split(c.in, ",")), ",")
		if got != c.want {
			t.Errorf("%q: %q, ожидалось %q", c.in, got, c.want)
		}
	}
}
