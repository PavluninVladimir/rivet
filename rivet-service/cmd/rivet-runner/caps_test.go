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
		{"deploy,docker", "deploy,docker"},
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

func TestDeliveryOnly(t *testing.T) {
	for _, s := range []string{"deploy", "deploy,docker"} {
		if !deliveryOnly(strings.Split(s, ",")) {
			t.Fatal(s)
		}
	}
	for _, s := range []string{"coding", "deploy,coding", "docker", ""} {
		if deliveryOnly(strings.Split(s, ",")) {
			t.Fatal(s)
		}
	}
}

func TestLoadCapabilityDoesNotAcquireArchitecture(t *testing.T) {
	got := withArchitectureCapability([]string{" load "})
	if len(got) != 1 || got[0] != "load" {
		t.Fatal(got)
	}
}
