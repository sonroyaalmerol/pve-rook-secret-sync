package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStagePendingKeyRequestsAES256K(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "ceph")
	args := filepath.Join(dir, "args")
	if err := os.WriteFile(script, []byte("args=$1\nshift\nprintf '%s\\n' \"$@\" >>\"$args\"\nprintf '%s\\n' '---' >>\"$args\"\nprintf '[{\"pending_key\":\"secret\"}]'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := newCephSource(cephConfig{Transport: "local", Command: []string{"sh", script, args}})
	if _, err := source.StagePendingKey(context.Background(), "client.rgw.test"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(args)
	if err != nil {
		t.Fatal(err)
	}
	want := "mon\nset\nauth_preferred_cipher\naes256k\n---\nauth\nget-or-create-pending\nclient.rgw.test\n--format\njson\n---\n"
	if string(got) != want {
		t.Fatalf("arguments = %q, want %q", got, want)
	}
}

func TestParseActiveManager(t *testing.T) {
	name, err := parseActiveManager([]byte(`{"active_name":"pve1"}`))
	if err != nil || name != "pve1" {
		t.Fatalf("got name %q and error %v", name, err)
	}
	if _, err := parseActiveManager([]byte(`{"active_name":""}`)); err == nil {
		t.Fatal("empty active manager was accepted")
	}
}

func TestParseMonConfig(t *testing.T) {
	data, err := parseMonConfig([]byte(`{"mons":[{"name":"pve1","public_addrs":{"addrvec":[{"type":"v2","addr":"10.0.0.1:3300/0"},{"type":"v1","addr":"10.0.0.1:6789/0"}]}},{"name":"pve2","public_addr":"10.0.0.2:6789/0"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if data["mon_initial_members"] != "pve1,pve2" {
		t.Fatalf("members = %q", data["mon_initial_members"])
	}
	if data["mon_host"] != "[v2:10.0.0.1:3300,v1:10.0.0.1:6789],[v1:10.0.0.2:6789]" {
		t.Fatalf("hosts = %q", data["mon_host"])
	}
	if _, err := parseMonConfig([]byte(`{"mons":[]}`)); err == nil {
		t.Fatal("empty monitor list was accepted")
	}
}

func TestParseDashboard(t *testing.T) {
	data, err := parseDashboard([]byte(`{"dashboard":"https://10.0.0.1:8443/"}`))
	if err != nil || data["url"] != "https://10.0.0.1:8443/" {
		t.Fatalf("data = %+v, error = %v", data, err)
	}
	if _, err := parseDashboard([]byte(`{}`)); err == nil {
		t.Fatal("missing dashboard was accepted")
	}
}

func TestParseRGWCredentials(t *testing.T) {
	data, err := parseRGWCredentials([]byte(`{"keys":[{"user":"rgw-admin-ops-user","access_key":"access","secret_key":"secret"}]}`), "rgw-admin-ops-user")
	if err != nil || data["accessKey"] != "access" || data["secretKey"] != "secret" {
		t.Fatalf("data = %+v, error = %v", data, err)
	}
	if _, err := parseRGWCredentials([]byte(`{"keys":[]}`), "rgw-admin-ops-user"); err == nil {
		t.Fatal("missing RGW key was accepted")
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

func TestSSHHostOptions(t *testing.T) {
	plain := strings.Join(sshHostOptions("vm-lan-1.example.com", ""), " ")
	if !strings.Contains(plain, "HostKeyAlias=vm-lan-1") || !strings.Contains(plain, "BatchMode=yes") {
		t.Fatalf("options = %q", plain)
	}
	if strings.Contains(plain, "UserKnownHostsFile") {
		t.Fatalf("missing host key file was still referenced: %q", plain)
	}
	cluster := strings.Join(sshHostOptions("vm-lan-1", "/etc/pve/nodes/vm-lan-1/ssh_known_hosts"), " ")
	if !strings.Contains(cluster, "UserKnownHostsFile=/etc/pve/nodes/vm-lan-1/ssh_known_hosts") || !strings.Contains(cluster, "GlobalKnownHostsFile=none") {
		t.Fatalf("options = %q", cluster)
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
