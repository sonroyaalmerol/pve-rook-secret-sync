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
	"time"
)

type vaultClient struct {
	address   string
	namespace string
	mount     string
	prefix    string
	token     string
	http      *http.Client
}

type vaultValue struct {
	Data    map[string]string
	Version int
	Exists  bool
}

func newVaultClient(cfg vaultConfig) (*vaultClient, error) {
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

	return &vaultClient{
		address:   cfg.Address,
		namespace: cfg.Namespace,
		mount:     cfg.Mount,
		prefix:    cfg.PathPrefix,
		token:     token,
		http: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (client *vaultClient) Read(ctx context.Context, path string) (vaultValue, error) {
	req, err := client.request(ctx, http.MethodGet, "data", path, nil)
	if err != nil {
		return vaultValue{}, err
	}
	resp, err := client.http.Do(req)
	if err != nil {
		return vaultValue{}, fmt.Errorf("read Vault path %s: %w", path, err)
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
	req, err := client.request(ctx, http.MethodPost, "data", path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.http.Do(req)
	if err != nil {
		return fmt.Errorf("write Vault path %s: %w", path, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("write Vault path %s: HTTP %s", path, resp.Status)
	}
	return nil
}

func (client *vaultClient) currentVersion(ctx context.Context, path string) (int, error) {
	req, err := client.request(ctx, http.MethodGet, "metadata", path, nil)
	if err != nil {
		return 0, err
	}
	resp, err := client.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("read Vault metadata %s: %w", path, err)
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
