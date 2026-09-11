package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

type vaultClient struct {
	address        string
	namespace      string
	mount          string
	prefix         string
	token          string
	cfg            vaultConfig
	auth           *vaultAuth
	tokenFromCache bool
	http           *http.Client
}

// vaultAuth caches login tokens across synchronization runs so polling does
// not authenticate against Vault every cycle. Callers may share one instance.
type vaultAuth struct {
	mu       sync.Mutex
	identity string
	token    string
	expiry   time.Time
}

func newVaultAuth() *vaultAuth {
	return &vaultAuth{}
}

func authIdentity(cfg vaultConfig) string {
	method, username, roleID := "token", "", ""
	mount := ""
	if cfg.Auth != nil {
		if cfg.Auth.Method != "" {
			method = cfg.Auth.Method
		}
		mount, username, roleID = cfg.Auth.Mount, cfg.Auth.Username, cfg.Auth.RoleID
	}
	return strings.Join([]string{cfg.Address, cfg.Namespace, method, mount, username, roleID}, "\x00")
}

type vaultValue struct {
	Data    map[string]string
	Version int
	Exists  bool
}

func newVaultClient(ctx context.Context, cfg vaultConfig, auth *vaultAuth) (*vaultClient, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.CACert != "" {
		pem, err := os.ReadFile(cfg.CACert)
		if err != nil {
			return nil, fmt.Errorf("read vault CA certificate: %w", err)
		}
		roots, err := x509.SystemCertPool()
		if err != nil {
			return nil, fmt.Errorf("load system certificate pool: %w", err)
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, errors.New("vault CA certificate contains no certificates")
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	}

	client := &vaultClient{
		address:   cfg.Address,
		namespace: cfg.Namespace,
		mount:     cfg.Mount,
		prefix:    cfg.PathPrefix,
		cfg:       cfg,
		auth:      auth,
		http: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
	if cfg.Auth == nil || cfg.Auth.Method == "" || cfg.Auth.Method == "token" {
		var token string
		if cfg.TokenFile != "" {
			data, err := os.ReadFile(cfg.TokenFile)
			if err != nil {
				return nil, fmt.Errorf("read vault token file: %w", err)
			}
			token = strings.TrimSpace(string(data))
			if token == "" {
				return nil, errors.New("vault token file is empty")
			}
		} else {
			token = os.Getenv(cfg.TokenEnv)
			if token == "" {
				return nil, fmt.Errorf("vault token environment variable %s is empty", cfg.TokenEnv)
			}
		}
		client.token = token
		return client, nil
	}
	if client.auth == nil {
		client.auth = newVaultAuth()
	}
	token, cached, err := client.auth.login(ctx, cfg, client.http, false)
	if err != nil {
		return nil, err
	}
	client.token = token
	client.tokenFromCache = cached
	return client, nil
}

func (a *vaultAuth) login(ctx context.Context, cfg vaultConfig, hc *http.Client, force bool) (token string, fromCache bool, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	identity := authIdentity(cfg)
	if !force && a.token != "" && a.identity == identity && time.Now().Before(a.expiry) {
		return a.token, true, nil
	}

	method := cfg.Auth.Method
	mount := cfg.Auth.Mount
	if mount == "" {
		mount = method
	}
	var endpoint string
	payload := map[string]string{}
	switch method {
	case "userpass":
		data, err := os.ReadFile(cfg.Auth.PasswordFile)
		if err != nil {
			return "", false, fmt.Errorf("read vault password file: %w", err)
		}
		password := strings.TrimSpace(string(data))
		if password == "" {
			return "", false, errors.New("vault password file is empty")
		}
		endpoint = cfg.Address + "/v1/auth/" + escapeVaultPath(mount) + "/login/" + url.PathEscape(cfg.Auth.Username)
		payload["password"] = password
	case "approle":
		data, err := os.ReadFile(cfg.Auth.SecretIDFile)
		if err != nil {
			return "", false, fmt.Errorf("read vault secret ID file: %w", err)
		}
		secretID := strings.TrimSpace(string(data))
		if secretID == "" {
			return "", false, errors.New("vault secret ID file is empty")
		}
		endpoint = cfg.Address + "/v1/auth/" + escapeVaultPath(mount) + "/login"
		payload["role_id"] = cfg.Auth.RoleID
		payload["secret_id"] = secretID
	default:
		return "", false, fmt.Errorf("unsupported vault auth method %q", method)
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", false, fmt.Errorf("encode vault %s login: %w", method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", false, fmt.Errorf("create vault %s login request: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.Namespace != "" {
		req.Header.Set("X-Vault-Namespace", cfg.Namespace)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", false, fmt.Errorf("vault %s login: %w", method, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return "", false, fmt.Errorf("vault %s login: HTTP %s", method, resp.Status)
	}
	var response struct {
		Auth struct {
			ClientToken   string `json:"client_token"`
			LeaseDuration int    `json:"lease_duration"`
		} `json:"auth"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&response); err != nil {
		return "", false, fmt.Errorf("decode vault %s login: %w", method, err)
	}
	if response.Auth.ClientToken == "" {
		return "", false, fmt.Errorf("vault %s login response missing client token", method)
	}
	guard := min(response.Auth.LeaseDuration/5, 60)
	a.token = response.Auth.ClientToken
	a.identity = identity
	a.expiry = time.Now().Add(time.Duration(response.Auth.LeaseDuration-guard) * time.Second)
	return a.token, false, nil
}

func (client *vaultClient) Read(ctx context.Context, path string) (vaultValue, error) {
	resp, err := client.do(ctx, http.MethodGet, "data", path, nil)
	if err != nil {
		return vaultValue{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, resp.Body)
		version, err := client.currentVersion(ctx, path)
		if err != nil {
			return vaultValue{}, err
		}
		return vaultValue{Version: version}, nil
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return vaultValue{}, fmt.Errorf("read Vault path %s: HTTP %s", path, resp.Status)
	}

	var payload struct {
		Data struct {
			Data     map[string]string `json:"data"`
			Metadata struct {
				Version int `json:"version"`
			} `json:"metadata"`
		} `json:"data"`
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 4<<20))
	if err := decoder.Decode(&payload); err != nil {
		return vaultValue{}, fmt.Errorf("decode Vault path %s: %w", path, err)
	}
	return vaultValue{Data: payload.Data.Data, Version: payload.Data.Metadata.Version, Exists: true}, nil
}

func (client *vaultClient) Write(ctx context.Context, path string, data map[string]string, version int) error {
	body, err := json.Marshal(struct {
		Data    map[string]string `json:"data"`
		Options struct {
			CAS int `json:"cas"`
		} `json:"options"`
	}{Data: data, Options: struct {
		CAS int `json:"cas"`
	}{CAS: version}})
	if err != nil {
		return fmt.Errorf("encode Vault path %s: %w", path, err)
	}
	resp, err := client.do(ctx, http.MethodPost, "data", path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("write Vault path %s: HTTP %s", path, resp.Status)
	}
	return nil
}

func (client *vaultClient) currentVersion(ctx context.Context, path string) (int, error) {
	resp, err := client.do(ctx, http.MethodGet, "metadata", path, nil)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, resp.Body)
		return 0, nil
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return 0, fmt.Errorf("read Vault metadata %s: HTTP %s", path, resp.Status)
	}

	var payload struct {
		Data struct {
			CurrentVersion int `json:"current_version"`
		} `json:"data"`
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 4<<20))
	if err := decoder.Decode(&payload); err != nil {
		return 0, fmt.Errorf("decode Vault metadata %s: %w", path, err)
	}
	if payload.Data.CurrentVersion < 1 {
		return 0, fmt.Errorf("decode Vault metadata %s: invalid current version", path)
	}
	return payload.Data.CurrentVersion, nil
}

func (client *vaultClient) do(ctx context.Context, method, endpointType, path string, body []byte) (*http.Response, error) {
	resp, err := client.attempt(ctx, method, endpointType, path, body)
	if err != nil {
		return nil, err
	}
	if (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) && client.auth != nil && client.tokenFromCache {
		token, _, loginErr := client.auth.login(ctx, client.cfg, client.http, true)
		if loginErr == nil {
			resp.Body.Close()
			client.token = token
			client.tokenFromCache = false
			return client.attempt(ctx, method, endpointType, path, body)
		}
	}
	return resp, nil
}

func (client *vaultClient) attempt(ctx context.Context, method, endpointType, path string, body []byte) (*http.Response, error) {
	req, err := client.request(ctx, method, endpointType, path, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.http.Do(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (client *vaultClient) request(ctx context.Context, method, endpointType, path string, body []byte) (*http.Request, error) {
	endpoint := client.address + "/v1/" + url.PathEscape(client.mount) + "/" + endpointType + "/" + escapeVaultPath(strings.Trim(client.prefix+"/"+path, "/"))
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create Vault request: %w", err)
	}
	req.Header.Set("X-Vault-Token", client.token)
	if client.namespace != "" {
		req.Header.Set("X-Vault-Namespace", client.namespace)
	}
	return req, nil
}

func escapeVaultPath(path string) string {
	parts := strings.Split(path, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}
