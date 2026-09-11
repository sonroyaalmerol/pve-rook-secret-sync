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
		"vault":{"mount":"secret","path_prefix":"rook/staging","token_file":"/etc/pve/priv/pve-rook-secret-sync/vault-token"},
		"credentials":[{"vault_path":"rook-ceph-mon","entity":"client.healthchecker","kind":"rook-mon"}]
	}`
	if err := os.WriteFile(name, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(name)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Ceph.Transport != "ssh" || cfg.Ceph.User != "root" || cfg.Ceph.Port != 22 || cfg.Ceph.Coordination != "active-manager" || cfg.Ceph.RGWPoolPrefix != "default" || len(cfg.Ceph.RGWCommand) != 1 || cfg.Ceph.RGWCommand[0] != "radosgw-admin" {
		t.Fatalf("unexpected Ceph defaults: %+v", cfg.Ceph)
	}
	if cfg.Vault.Address != "https://vault.example.com" || cfg.Vault.TokenEnv != "VAULT_TOKEN" || cfg.Vault.TokenFile != "/etc/pve/priv/pve-rook-secret-sync/vault-token" {
		t.Fatalf("unexpected Vault defaults: %+v", cfg.Vault)
	}
	if cfg.RookClusterName != "rook-ceph" {
		t.Fatalf("unexpected cluster name %q", cfg.RookClusterName)
	}
}

func TestConfigAuthDefaults(t *testing.T) {
	cfg := config{
		Ceph:            cephConfig{Transport: "local", Port: 22, Command: []string{"ceph"}, RGWCommand: []string{"radosgw-admin"}, MigrationHelper: []string{"pve-cephx-rotate-service-keys"}, Coordination: "active-manager"},
		Vault:           vaultConfig{Address: "https://vault.example.com", Mount: "secret", PathPrefix: "rook/staging", Auth: &vaultAuthConfig{Method: "userpass", Username: "alice", PasswordFile: "/pw"}},
		RookClusterName: "rook-ceph",
		Credentials:     []credentialSpec{{VaultPath: "mon", Entity: "client.healthchecker", Kind: "rook-mon"}},
	}
	cfg.applyDefaults()
	if cfg.Vault.Auth.Mount != "userpass" {
		t.Fatalf("auth mount = %q, want userpass", cfg.Vault.Auth.Mount)
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("valid userpass config rejected: %v", err)
	}
	cfg.Vault.Auth = &vaultAuthConfig{RoleID: "role", SecretIDFile: "/sid"}
	cfg.applyDefaults()
	if cfg.Vault.Auth.Method != "token" || cfg.Vault.Auth.Mount != "token" {
		t.Fatalf("unexpected defaults: %+v", cfg.Vault.Auth)
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("implicit token method rejected: %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	base := config{
		Ceph:            cephConfig{Transport: "local", Port: 22, Command: []string{"ceph"}, RGWCommand: []string{"radosgw-admin"}, MigrationHelper: []string{"pve-cephx-rotate-service-keys"}, Coordination: "active-manager"},
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
		{"non-RGW daemon entity", func(cfg *config) {
			cfg.Ceph.RGWDaemons = []rgwDaemonConfig{{Entity: "client.admin", Host: "pve1", Unit: "rgw.service", Keyring: "/etc/ceph/keyring"}}
		}, "client.rgw entity"},
		{"relative daemon keyring", func(cfg *config) {
			cfg.Ceph.RGWDaemons = []rgwDaemonConfig{{Entity: "client.rgw.one", Host: "pve1", Unit: "rgw.service", Keyring: "keyring"}}
		}, "keyring must be an absolute path"},
		{"duplicate daemon entity", func(cfg *config) {
			daemon := rgwDaemonConfig{Entity: "client.rgw.one", Host: "pve1", Unit: "rgw.service", Keyring: "/etc/ceph/keyring"}
			cfg.Ceph.RGWDaemons = []rgwDaemonConfig{daemon, daemon}
		}, "duplicate ceph.rgw_daemons"},
		{"negative CephX generation", func(cfg *config) { cfg.CephXGeneration = -1 }, "cephx_generation"},
		{"remote HTTP", func(cfg *config) { cfg.Vault.Address = "http://vault.example.com" }, "must use HTTPS"},
		{"URL path", func(cfg *config) { cfg.Vault.Address = "https://vault.example.com/proxy" }, "must not include"},
		{"URL query", func(cfg *config) { cfg.Vault.Address = "https://vault.example.com?target=other" }, "must not include"},
		{"parent path", func(cfg *config) { cfg.Vault.PathPrefix = "rook/../other" }, "parent segments"},
		{"duplicate path", func(cfg *config) { cfg.Credentials = append(cfg.Credentials, cfg.Credentials[0]) }, "duplicate"},
		{"unknown kind", func(cfg *config) { cfg.Credentials[0].Kind = "generic" }, "kind must be"},
		{"missing entity", func(cfg *config) { cfg.Credentials[0].Entity = "" }, "entity is required"},
		{"unknown auth method", func(cfg *config) { cfg.Vault.Auth = &vaultAuthConfig{Method: "ldap"} }, "method must be"},
		{"userpass without username", func(cfg *config) { cfg.Vault.Auth = &vaultAuthConfig{Method: "userpass", PasswordFile: "/pw"} }, "username is required"},
		{"userpass without password file", func(cfg *config) { cfg.Vault.Auth = &vaultAuthConfig{Method: "userpass", Username: "alice"} }, "password_file is required"},
		{"approle without role ID", func(cfg *config) { cfg.Vault.Auth = &vaultAuthConfig{Method: "approle", SecretIDFile: "/sid"} }, "role_id is required"},
		{"approle without secret ID file", func(cfg *config) { cfg.Vault.Auth = &vaultAuthConfig{Method: "approle", RoleID: "role"} }, "secret_id_file is required"},
		{"auth mount traversal", func(cfg *config) {
			cfg.Vault.Auth = &vaultAuthConfig{Method: "userpass", Username: "alice", PasswordFile: "/pw", Mount: "../auth"}
		}, "vault.auth.mount"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := base
			cfg.Credentials = append([]credentialSpec(nil), base.Credentials...)
			cfg.Ceph.RGWDaemons = nil
			test.change(&cfg)
			err := cfg.validate()
			if err == nil || !strings.Contains(err.Error(), test.match) {
				t.Fatalf("got %v, want error containing %q", err, test.match)
			}
		})
	}
}

func TestConfigAllowsEntitylessMetadata(t *testing.T) {
	cfg := config{
		Ceph:            cephConfig{Transport: "local", Port: 22, Command: []string{"ceph"}, RGWCommand: []string{"radosgw-admin"}, MigrationHelper: []string{"pve-cephx-rotate-service-keys"}, Coordination: "active-manager"},
		Vault:           vaultConfig{Address: "https://vault.example.com", Mount: "secret", PathPrefix: "rook/staging"},
		RookClusterName: "rook-ceph",
		Credentials: []credentialSpec{
			{VaultPath: "config", Kind: "rook-config"},
			{VaultPath: "dashboard", Kind: "rook-dashboard"},
		},
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("entityless metadata config rejected: %v", err)
	}
}
