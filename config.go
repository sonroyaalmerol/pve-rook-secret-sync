package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type config struct {
	Ceph            cephConfig       `json:"ceph"`
	Vault           vaultConfig      `json:"vault"`
	RookClusterName string           `json:"rook_cluster_name"`
	Credentials     []credentialSpec `json:"credentials"`
}

type cephConfig struct {
	Transport    string   `json:"transport"`
	Host         string   `json:"host,omitempty"`
	User         string   `json:"user,omitempty"`
	Port         int      `json:"port,omitempty"`
	Command      []string `json:"command"`
	Coordination string   `json:"coordination"`
	ManagerName  string   `json:"manager_name,omitempty"`
}

type vaultConfig struct {
	Address    string `json:"address"`
	Namespace  string `json:"namespace,omitempty"`
	Mount      string `json:"mount"`
	PathPrefix string `json:"path_prefix"`
	TokenEnv   string `json:"token_env,omitempty"`
	TokenFile  string `json:"token_file,omitempty"`
	CACert     string `json:"ca_cert,omitempty"`
	AllowHTTP  bool   `json:"allow_http,omitempty"`
}

type credentialSpec struct {
	VaultPath string `json:"vault_path"`
	Entity    string `json:"entity"`
	Kind      string `json:"kind"`
	UserID    string `json:"user_id,omitempty"`
}

func loadConfig(name string) (config, error) {
	f, err := os.Open(name)
	if err != nil {
		return config{}, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()

	var cfg config
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return config{}, fmt.Errorf("decode config: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return config{}, errors.New("decode config: trailing JSON value")
	}

	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return config{}, err
	}
	return cfg, nil
}

func (cfg *config) applyDefaults() {
	if cfg.Ceph.Transport == "" {
		if cfg.Ceph.Host == "" {
			cfg.Ceph.Transport = "local"
		} else {
			cfg.Ceph.Transport = "ssh"
		}
	}
	if cfg.Ceph.User == "" {
		cfg.Ceph.User = "root"
	}
	if cfg.Ceph.Port == 0 {
		cfg.Ceph.Port = 22
	}
	if len(cfg.Ceph.Command) == 0 {
		cfg.Ceph.Command = []string{"ceph"}
	}
	if cfg.Ceph.Coordination == "" {
		cfg.Ceph.Coordination = "active-manager"
	}
	if cfg.Vault.Address == "" {
		cfg.Vault.Address = os.Getenv("VAULT_ADDR")
	}
	cfg.Vault.Address = strings.TrimRight(cfg.Vault.Address, "/")
	if cfg.Vault.Namespace == "" {
		cfg.Vault.Namespace = os.Getenv("VAULT_NAMESPACE")
	}
	if cfg.Vault.TokenEnv == "" {
		cfg.Vault.TokenEnv = "VAULT_TOKEN"
	}
	if cfg.Vault.CACert == "" {
		cfg.Vault.CACert = os.Getenv("VAULT_CACERT")
	}
	if cfg.RookClusterName == "" {
		cfg.RookClusterName = "rook-ceph"
	}
	cfg.Vault.Mount = strings.Trim(cfg.Vault.Mount, "/")
	cfg.Vault.PathPrefix = strings.Trim(cfg.Vault.PathPrefix, "/")
}

func (cfg config) validate() error {
	if cfg.Ceph.Transport != "local" && cfg.Ceph.Transport != "ssh" {
		return errors.New("ceph.transport must be local or ssh")
	}
	if cfg.Ceph.Transport == "ssh" && cfg.Ceph.Host == "" {
		return errors.New("ceph.host is required for ssh transport")
	}
	if cfg.Ceph.Port < 1 || cfg.Ceph.Port > 65535 {
		return errors.New("ceph.port must be between 1 and 65535")
	}
	if len(cfg.Ceph.Command) == 0 || cfg.Ceph.Command[0] == "" {
		return errors.New("ceph.command must not be empty")
	}
	if cfg.Ceph.Coordination != "active-manager" && cfg.Ceph.Coordination != "none" {
		return errors.New("ceph.coordination must be active-manager or none")
	}
	if cfg.Vault.Address == "" {
		return errors.New("vault.address or VAULT_ADDR is required")
	}
	u, err := url.Parse(cfg.Vault.Address)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return errors.New("vault.address must be an absolute HTTP URL")
	}
	if u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("vault.address must not include user info, a path, query, or fragment")
	}
	if u.Scheme == "http" && !cfg.Vault.AllowHTTP && u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" {
		return errors.New("vault.address must use HTTPS unless allow_http is true")
	}
	if cfg.Vault.Mount == "" {
		return errors.New("vault.mount is required")
	}
	if err := validateVaultPath(cfg.Vault.Mount); err != nil {
		return fmt.Errorf("vault.mount: %w", err)
	}
	if cfg.Vault.PathPrefix == "" {
		return errors.New("vault.path_prefix is required")
	}
	if err := validateVaultPath(cfg.Vault.PathPrefix); err != nil {
		return fmt.Errorf("vault.path_prefix: %w", err)
	}
	if cfg.RookClusterName == "" {
		return errors.New("rook_cluster_name is required")
	}
	if len(cfg.Credentials) == 0 {
		return errors.New("at least one credential is required")
	}

	paths := make(map[string]struct{}, len(cfg.Credentials))
	for i, credential := range cfg.Credentials {
		if credential.Entity == "" {
			return fmt.Errorf("credentials[%d].entity is required", i)
		}
		if credential.VaultPath == "" {
			return fmt.Errorf("credentials[%d].vault_path is required", i)
		}
		if err := validateVaultPath(credential.VaultPath); err != nil {
			return fmt.Errorf("credentials[%d].vault_path: %w", i, err)
		}
		if _, exists := paths[credential.VaultPath]; exists {
			return fmt.Errorf("duplicate credential vault_path %q", credential.VaultPath)
		}
		paths[credential.VaultPath] = struct{}{}
		switch credential.Kind {
		case "rook-mon", "rook-csi", "rook-cephfs-csi":
		default:
			return fmt.Errorf("credentials[%d].kind must be rook-mon, rook-csi, or rook-cephfs-csi", i)
		}
	}
	return nil
}

func validateVaultPath(value string) error {
	if value == "." || value == ".." || filepath.IsAbs(value) {
		return errors.New("must be a relative path")
	}
	for part := range strings.SplitSeq(value, "/") {
		if part == "" || part == "." || part == ".." {
			return errors.New("must not contain empty, dot, or parent segments")
		}
	}
	return nil
}
