package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestVaultReadAndWrite(t *testing.T) {
	var write struct {
		Data    map[string]string `json:"data"`
		Options struct {
			CAS int `json:"cas"`
		} `json:"options"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/service-secrets/data/rook/staging/mon" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if r.Header.Get("X-Vault-Token") != "token" || r.Header.Get("X-Vault-Namespace") != "team" {
			t.Error("missing Vault headers")
		}
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"data":{"data":{"userKey":"secret"},"metadata":{"version":7}}}`))
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&write); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := &vaultClient{
		address: server.URL, namespace: "team", mount: "service-secrets", prefix: "rook/staging", token: "token", http: server.Client(),
	}
	value, err := client.Read(context.Background(), "mon")
	if err != nil {
		t.Fatal(err)
	}
	if !value.Exists || value.Version != 7 || value.Data["userKey"] != "secret" {
		t.Fatalf("unexpected value: %+v", value)
	}
	if err := client.Write(context.Background(), "mon", map[string]string{"userKey": "new-secret"}, 7); err != nil {
		t.Fatal(err)
	}
	if write.Options.CAS != 7 || write.Data["userKey"] != "new-secret" {
		t.Fatalf("unexpected write: %+v", write)
	}
}

func TestVaultReadDeletedSecretUsesMetadataVersion(t *testing.T) {
	var cas int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/data/"):
			http.NotFound(w, r)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/metadata/"):
			_, _ = w.Write([]byte(`{"data":{"current_version":7}}`))
		case r.Method == http.MethodPost:
			var payload struct {
				Options struct {
					CAS int `json:"cas"`
				} `json:"options"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			cas = payload.Options.CAS
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	client := &vaultClient{address: server.URL, mount: "secret", prefix: "prefix", token: "token", http: server.Client()}
	value, err := client.Read(context.Background(), "key")
	if err != nil {
		t.Fatal(err)
	}
	if value.Exists || value.Version != 7 {
		t.Fatalf("unexpected deleted value: %+v", value)
	}
	if err := client.Write(context.Background(), "key", map[string]string{"value": "new"}, value.Version); err != nil {
		t.Fatal(err)
	}
	if cas != 7 {
		t.Fatalf("write CAS = %d, want 7", cas)
	}
}

func TestVaultClientReadsTokenFile(t *testing.T) {
	name := filepath.Join(t.TempDir(), "vault-token")
	if err := os.WriteFile(name, []byte("shared-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_VAULT_TOKEN", "environment-token")
	client, err := newVaultClient(context.Background(), vaultConfig{
		Address: "https://vault.example.com", Mount: "secret", PathPrefix: "prefix", TokenEnv: "TEST_VAULT_TOKEN", TokenFile: name,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	client.http.CloseIdleConnections()
	if client.token != "shared-token" {
		t.Fatalf("token = %q, want shared token", client.token)
	}
}

func TestVaultClientRejectsRedirects(t *testing.T) {
	redirected := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected = true
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer server.Close()

	t.Setenv("TEST_VAULT_TOKEN", "secret")
	client, err := newVaultClient(context.Background(), vaultConfig{
		Address: server.URL, Mount: "secret", PathPrefix: "prefix", TokenEnv: "TEST_VAULT_TOKEN", AllowHTTP: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Read(context.Background(), "key"); err == nil || !strings.Contains(err.Error(), "HTTP 302") {
		t.Fatalf("got %v, want redirect error", err)
	}
	if redirected {
		t.Fatal("Vault token was sent to redirect target")
	}
}

func TestVaultClientUserpassLoginAndCache(t *testing.T) {
	passwordFile := filepath.Join(t.TempDir(), "vault-password")
	if err := os.WriteFile(passwordFile, []byte("hunter2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	logins := 0
	var loginBody map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/auth/userpass/login/alice":
			if r.Header.Get("X-Vault-Token") != "" {
				t.Error("login request must not send a token")
			}
			if err := json.NewDecoder(r.Body).Decode(&loginBody); err != nil {
				t.Error(err)
			}
			logins++
			_, _ = w.Write(fmt.Appendf(nil, `{"auth":{"client_token":"tok%d","lease_duration":3600}}`, logins))
		case r.URL.Path == "/v1/secret/data/rook/mon":
			if r.Header.Get("X-Vault-Token") == "" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			_, _ = w.Write([]byte(`{"data":{"data":{"key":"value"},"metadata":{"version":1}}}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	cfg := vaultConfig{
		Address: server.URL, Mount: "secret", PathPrefix: "rook", AllowHTTP: true,
		Auth: &vaultAuthConfig{Method: "userpass", Username: "alice", PasswordFile: passwordFile},
	}
	auth := newVaultAuth()
	first, err := newVaultClient(context.Background(), cfg, auth)
	if err != nil {
		t.Fatal(err)
	}
	defer first.http.CloseIdleConnections()
	if loginBody["password"] != "hunter2" || first.token != "tok1" || first.tokenFromCache {
		t.Fatalf("login body %v, token %q, cached %v", loginBody, first.token, first.tokenFromCache)
	}

	second, err := newVaultClient(context.Background(), cfg, auth)
	if err != nil {
		t.Fatal(err)
	}
	defer second.http.CloseIdleConnections()
	if second.token != "tok1" || !second.tokenFromCache || logins != 1 {
		t.Fatalf("token %q, cached %v, logins %d", second.token, second.tokenFromCache, logins)
	}

	auth.mu.Lock()
	auth.expiry = time.Now().Add(-time.Minute)
	auth.mu.Unlock()
	third, err := newVaultClient(context.Background(), cfg, auth)
	if err != nil {
		t.Fatal(err)
	}
	defer third.http.CloseIdleConnections()
	if third.token != "tok2" || logins != 2 {
		t.Fatalf("token %q, logins %d", third.token, logins)
	}
	if _, err := third.Read(context.Background(), "mon"); err != nil {
		t.Fatal(err)
	}
}

func TestVaultClientApproleLogin(t *testing.T) {
	secretFile := filepath.Join(t.TempDir(), "vault-secret-id")
	if err := os.WriteFile(secretFile, []byte("sid-123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var loginBody map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/auth/approle/login":
			if err := json.NewDecoder(r.Body).Decode(&loginBody); err != nil {
				t.Error(err)
			}
			_, _ = w.Write([]byte(`{"auth":{"client_token":"app-token","lease_duration":1200}}`))
		case r.URL.Path == "/v1/secret/data/rook/mon":
			if r.Header.Get("X-Vault-Token") != "app-token" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			_, _ = w.Write([]byte(`{"data":{"data":{"key":"value"},"metadata":{"version":1}}}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	client, err := newVaultClient(context.Background(), vaultConfig{
		Address: server.URL, Mount: "secret", PathPrefix: "rook", AllowHTTP: true,
		Auth: &vaultAuthConfig{Method: "approle", RoleID: "role-1", SecretIDFile: secretFile},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.http.CloseIdleConnections()
	if loginBody["role_id"] != "role-1" || loginBody["secret_id"] != "sid-123" {
		t.Fatalf("unexpected login body: %v", loginBody)
	}
	if _, err := client.Read(context.Background(), "mon"); err != nil {
		t.Fatal(err)
	}
}

func TestVaultClientReloginsWhenCachedTokenRevoked(t *testing.T) {
	passwordFile := filepath.Join(t.TempDir(), "vault-password")
	if err := os.WriteFile(passwordFile, []byte("pw"), 0o600); err != nil {
		t.Fatal(err)
	}
	logins := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/auth/userpass/login/alice":
			logins++
			_, _ = w.Write(fmt.Appendf(nil, `{"auth":{"client_token":"tok%d","lease_duration":3600}}`, logins))
		case r.URL.Path == "/v1/secret/data/rook/mon":
			if r.Header.Get("X-Vault-Token") != "tok2" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			_, _ = w.Write([]byte(`{"data":{"data":{"key":"value"},"metadata":{"version":1}}}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	cfg := vaultConfig{
		Address: server.URL, Mount: "secret", PathPrefix: "rook", AllowHTTP: true,
		Auth: &vaultAuthConfig{Method: "userpass", Username: "alice", PasswordFile: passwordFile},
	}
	auth := newVaultAuth()
	first, err := newVaultClient(context.Background(), cfg, auth)
	if err != nil {
		t.Fatal(err)
	}
	defer first.http.CloseIdleConnections()

	second, err := newVaultClient(context.Background(), cfg, auth)
	if err != nil {
		t.Fatal(err)
	}
	defer second.http.CloseIdleConnections()
	if _, err := second.Read(context.Background(), "mon"); err != nil {
		t.Fatalf("read with revoked cached token: %v", err)
	}
	if logins != 2 {
		t.Fatalf("logins = %d, want 2", logins)
	}
}
