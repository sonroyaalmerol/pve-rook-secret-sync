package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	t.Setenv("VAULT_ADDR", "https://vault.example.com")
	name := filepath.Join(t.TempDir(), "config.json")
	body := `{
		"ceph":{"host":"pve1"},
		"vault":{"mount":"secret","path_prefix":"rook/staging","token_file":"/etc/pve/priv/ceph-vault-sync/vault-token"},
		"credentials":[{"vault_path":"rook-ceph-mon","entity":"client.healthchecker","kind":"rook-mon"}]
	}`
	if err := os.WriteFile(name, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(name)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Ceph.Transport != "ssh" || cfg.Ceph.User != "root" || cfg.Ceph.Port != 22 || cfg.Ceph.Coordination != "active-manager" {
		t.Fatalf("unexpected Ceph defaults: %+v", cfg.Ceph)
	}
	if cfg.Vault.Address != "https://vault.example.com" || cfg.Vault.TokenEnv != "VAULT_TOKEN" || cfg.Vault.TokenFile != "/etc/pve/priv/ceph-vault-sync/vault-token" {
		t.Fatalf("unexpected Vault defaults: %+v", cfg.Vault)
	}
	if cfg.RookClusterName != "rook-ceph" {
		t.Fatalf("unexpected cluster name %q", cfg.RookClusterName)
	}
}

func TestConfigValidation(t *testing.T) {
	base := config{
		Ceph:            cephConfig{Transport: "local", Port: 22, Command: []string{"ceph"}, Coordination: "active-manager"},
		Vault:           vaultConfig{Address: "https://vault.example.com", Mount: "secret", PathPrefix: "rook/staging"},
		RookClusterName: "rook-ceph",
		Credentials:     []credentialSpec{{VaultPath: "mon", Entity: "client.healthchecker", Kind: "rook-mon"}},
	}
	tests := []struct {
		name   string
		change func(*config)
		match  string
	}{
		{"unknown coordination", func(cfg *config) { cfg.Ceph.Coordination = "all-hosts" }, "coordination must be"},
		{"remote HTTP", func(cfg *config) { cfg.Vault.Address = "http://vault.example.com" }, "must use HTTPS"},
		{"URL path", func(cfg *config) { cfg.Vault.Address = "https://vault.example.com/proxy" }, "must not include"},
		{"URL query", func(cfg *config) { cfg.Vault.Address = "https://vault.example.com?target=other" }, "must not include"},
		{"parent path", func(cfg *config) { cfg.Vault.PathPrefix = "rook/../other" }, "parent segments"},
		{"duplicate path", func(cfg *config) { cfg.Credentials = append(cfg.Credentials, cfg.Credentials[0]) }, "duplicate"},
		{"unknown kind", func(cfg *config) { cfg.Credentials[0].Kind = "generic" }, "kind must be"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := base
			cfg.Credentials = append([]credentialSpec(nil), base.Credentials...)
			test.change(&cfg)
			err := cfg.validate()
			if err == nil || !strings.Contains(err.Error(), test.match) {
				t.Fatalf("got %v, want error containing %q", err, test.match)
			}
		})
	}
}
