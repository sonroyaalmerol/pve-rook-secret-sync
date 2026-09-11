package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	client, err := newVaultClient(vaultConfig{
		Address: "https://vault.example.com", Mount: "secret", PathPrefix: "prefix", TokenEnv: "TEST_VAULT_TOKEN", TokenFile: name,
	})
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
	client, err := newVaultClient(vaultConfig{
		Address: server.URL, Mount: "secret", PathPrefix: "prefix", TokenEnv: "TEST_VAULT_TOKEN", AllowHTTP: true,
	})
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
