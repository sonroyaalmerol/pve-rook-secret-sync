package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
