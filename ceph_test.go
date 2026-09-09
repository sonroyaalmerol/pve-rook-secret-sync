package main

import "testing"

func TestParseActiveManager(t *testing.T) {
	name, err := parseActiveManager([]byte(`{"active_name":"pve1"}`))
	if err != nil || name != "pve1" {
		t.Fatalf("got name %q and error %v", name, err)
	}
	if _, err := parseActiveManager([]byte(`{"active_name":""}`)); err == nil {
		t.Fatal("empty active manager was accepted")
	}
}

func TestManagerNamesMatch(t *testing.T) {
	tests := []struct {
		local  string
		active string
		want   bool
	}{
		{"pve1", "pve1", true},
		{"pve1.example.com", "pve1", true},
		{"pve1", "mgr.pve1.example.com", true},
		{"PVE1", "pve1", true},
		{"pve1", "pve2", false},
		{"10.0.0.1", "10.0.0.2", false},
	}
	for _, test := range tests {
		if got := managerNamesMatch(test.local, test.active); got != test.want {
			t.Errorf("managerNamesMatch(%q, %q) = %v, want %v", test.local, test.active, got, test.want)
		}
	}
}

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
