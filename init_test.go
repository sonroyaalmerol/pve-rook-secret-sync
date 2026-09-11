package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunInitWritesConfig(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "shared", "config.json")
	token := filepath.Join(dir, "shared", "vault-token")
	var stdout, stderr bytes.Buffer
	args := []string{
		"init",
		"-vault-address", "https://vault.example.com:8200",
		"-vault-namespace", "team",
		"-path-prefix", "rook-pve/production",
		"-token-file", token,
		"-output", output,
	}
	if code := run(context.Background(), args, &stdout, &stderr, nil); code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
	}
	cfg, err := loadConfig(output)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Ceph.Transport != "local" || cfg.Ceph.Coordination != "active-manager" {
		t.Fatalf("unexpected Ceph config: %+v", cfg.Ceph)
	}
	if cfg.Vault.Address != "https://vault.example.com:8200" || cfg.Vault.Namespace != "team" || cfg.Vault.Mount != "service-secrets" || cfg.Vault.PathPrefix != "rook-pve/production" || cfg.Vault.TokenFile != token {
		t.Fatalf("unexpected Vault config: %+v", cfg.Vault)
	}
	if cfg.RookClusterName != "rook-ceph" || len(cfg.Credentials) != 5 {
		t.Fatalf("unexpected Rook config: cluster=%q credentials=%d", cfg.RookClusterName, len(cfg.Credentials))
	}
	info, err := os.Stat(output)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config permissions = %o, want 600", info.Mode().Perm())
	}
	if _, err := os.Stat(token); !os.IsNotExist(err) {
		t.Fatalf("token file was created: %v", err)
	}
	if !strings.Contains(stdout.String(), token) {
		t.Fatalf("stdout does not mention token path: %q", stdout.String())
	}
}

func TestRunInitRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "config.json")
	if err := os.WriteFile(output, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	args := []string{
		"init",
		"-vault-address", "https://vault.example.com",
		"-path-prefix", "rook-pve/production",
		"-token-file", filepath.Join(dir, "vault-token"),
		"-output", output,
	}
	if code := run(context.Background(), args, io.Discard, &stderr, nil); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	body, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "keep" || !strings.Contains(stderr.String(), "file exists") {
		t.Fatalf("body = %q, stderr = %q", body, stderr.String())
	}
}

func TestRunInitHelp(t *testing.T) {
	var stderr bytes.Buffer
	if code := run(context.Background(), []string{"init", "-h"}, io.Discard, &stderr, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stderr.String(), "-vault-address") {
		t.Fatalf("unexpected help %q", stderr.String())
	}
}

func TestRunInitUserpassAuth(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "config.json")
	password := filepath.Join(dir, "vault-password")
	var stdout, stderr bytes.Buffer
	args := []string{
		"init",
		"-vault-address", "https://vault.example.com:8200",
		"-path-prefix", "rook-pve/production",
		"-auth-method", "userpass",
		"-auth-username", "ceph-sync",
		"-auth-password-file", password,
		"-output", output,
	}
	if code := run(context.Background(), args, &stdout, &stderr, nil); code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
	}
	body, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "token_env") || strings.Contains(string(body), "token_file") {
		t.Fatalf("written config still contains token fields: %s", body)
	}
	cfg, err := loadConfig(output)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Vault.TokenFile != "" {
		t.Fatalf("token file should be omitted: %+v", cfg.Vault)
	}
	if cfg.Vault.Auth == nil || cfg.Vault.Auth.Method != "userpass" || cfg.Vault.Auth.Mount != "userpass" || cfg.Vault.Auth.Username != "ceph-sync" || cfg.Vault.Auth.PasswordFile != password {
		t.Fatalf("unexpected auth config: %+v", cfg.Vault.Auth)
	}
	if _, err := os.Stat(password); !os.IsNotExist(err) {
		t.Fatalf("password file was created: %v", err)
	}
	if !strings.Contains(stdout.String(), password) {
		t.Fatalf("stdout does not mention password path: %q", stdout.String())
	}
}

func TestRunInitRejectsBadAuthMethod(t *testing.T) {
	var stderr bytes.Buffer
	args := []string{"init", "-vault-address", "https://vault.example.com", "-path-prefix", "p", "-auth-method", "ldap"}
	if code := run(context.Background(), args, io.Discard, &stderr, nil); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "-auth-method") {
		t.Fatalf("unexpected stderr %q", stderr.String())
	}
}

func TestRunInitRequiresPathPrefix(t *testing.T) {
	var stderr bytes.Buffer
	args := []string{"init", "-vault-address", "https://vault.example.com"}
	if code := run(context.Background(), args, io.Discard, &stderr, nil); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "-path-prefix is required") {
		t.Fatalf("unexpected stderr %q", stderr.String())
	}
}
