package main

import "testing"

func TestShellQuote(t *testing.T) {
	tests := map[string]string{
		"":                  "''",
		"ceph":              "'ceph'",
		"client.test":       "'client.test'",
		"value with spaces": "'value with spaces'",
		"a'b":               "'a'\"'\"'b'",
	}
	for input, want := range tests {
		if got := shellQuote(input); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", input, got, want)
		}
	}
}
