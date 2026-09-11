package main

import (
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

//go:embed config.example.json
var exampleConfig []byte

func runInit(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	flags.SetOutput(stderr)
	address := flags.String("vault-address", os.Getenv("VAULT_ADDR"), "Vault server URL")
	namespace := flags.String("vault-namespace", os.Getenv("VAULT_NAMESPACE"), "Vault namespace")
	mount := flags.String("vault-mount", "service-secrets", "Vault KV v2 mount")
	pathPrefix := flags.String("path-prefix", "", "Vault path prefix for this cluster")
	caCert := flags.String("ca-cert", os.Getenv("VAULT_CACERT"), "Vault CA certificate path")
	rookClusterName := flags.String("rook-cluster-name", "rook-ceph", "Rook cluster name")
	tokenFile := flags.String("token-file", sharedTokenPath, "Vault token file referenced by the config")
	output := flags.String("output", sharedConfigPath, "configuration file to create")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "unexpected argument %q\n", flags.Arg(0))
		return 2
	}
	if *address == "" {
		fmt.Fprintln(stderr, "-vault-address or VAULT_ADDR is required")
		return 2
	}
	if *pathPrefix == "" {
		fmt.Fprintln(stderr, "-path-prefix is required")
		return 2
	}
	if *tokenFile == "" {
		fmt.Fprintln(stderr, "-token-file must not be empty")
		return 2
	}
	if *output == "" {
		fmt.Fprintln(stderr, "-output must not be empty")
		return 2
	}
	if *output == sharedConfigPath {
		info, err := os.Stat(pvePrivatePath)
		if err != nil {
			fmt.Fprintf(stderr, "PVE cluster filesystem is unavailable: %v\n", err)
			return 1
		}
		if !info.IsDir() {
			fmt.Fprintln(stderr, "PVE private path is not a directory")
			return 1
		}
	}

	var cfg config
	if err := json.Unmarshal(exampleConfig, &cfg); err != nil {
		fmt.Fprintf(stderr, "decode embedded configuration: %v\n", err)
		return 1
	}
	cfg.Vault.Address = *address
	cfg.Vault.Namespace = *namespace
	cfg.Vault.Mount = *mount
	cfg.Vault.PathPrefix = *pathPrefix
	cfg.Vault.TokenEnv = ""
	cfg.Vault.TokenFile = *tokenFile
	cfg.Vault.CACert = *caCert
	cfg.RookClusterName = *rookClusterName
	validated := cfg
	validated.applyDefaults()
	if err := validated.validate(); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := writeNewConfig(*output, cfg); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "wrote %s\n", *output)
	fmt.Fprintf(stdout, "write the Vault token to %s before starting the service\n", *tokenFile)
	return 0
}

func writeNewConfig(name string, cfg config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create config: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return fmt.Errorf("write config: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("close config: %w", err)
	}
	return nil
}
