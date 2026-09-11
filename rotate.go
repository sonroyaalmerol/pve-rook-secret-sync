package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type rotationSource interface {
	cephReader
	CreateKey(context.Context, string, []string, string) error
	Version(context.Context) (cephVersion, error)
}

type cephVersion struct {
	Major int
	Minor int
	Patch int
}

func runRotate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("rotate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "", "path to JSON configuration")
	dryRun := flags.Bool("dry-run", false, "show the migration without creating users or writing Vault")
	timeout := flags.Duration("timeout", 2*time.Minute, "overall operation timeout")
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
	if *timeout <= 0 {
		fmt.Fprintln(stderr, "-timeout must be positive")
		return 2
	}
	path := *configPath
	if path == "" {
		path = preferredConfigPath(sharedConfigPath, localConfigPath)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	auth := newVaultAuth()
	client, err := newVaultClient(ctx, cfg.Vault, auth)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer client.http.CloseIdleConnections()

	next, err := rotateCredentials(ctx, cfg, newCephSource(cfg.Ceph), client, *dryRun, stdout)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if !*dryRun {
		if err := replaceConfig(path, next); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintf(stdout, "%s: active CephX generation is %d\n", path, next.CephXGeneration)
	}
	return 0
}

func rotateCredentials(ctx context.Context, cfg config, source rotationSource, vault vaultStore, dryRun bool, output io.Writer) (config, error) {
	eligible, activeManager, err := source.CanSynchronize(ctx)
	if err != nil {
		return cfg, err
	}
	if !eligible {
		return cfg, fmt.Errorf("rotation must run on active ceph manager %s", activeManager)
	}
	version, err := source.Version(ctx)
	if err != nil {
		return cfg, err
	}
	if version.less(cephVersion{Major: 19, Minor: 2, Patch: 6}) {
		return cfg, fmt.Errorf("AES256K rotation requires Ceph 19.2.6 or newer; found %d.%d.%d", version.Major, version.Minor, version.Patch)
	}

	next := cfg
	next.CephXGeneration++
	rotatable := 0
	for _, credential := range cfg.Credentials {
		caps, ok := rookCephXProfile(credential.Entity, cfg.Ceph.RGWPoolPrefix)
		if !ok {
			continue
		}
		rotatable++
		entity := activeCredential(credential, next.CephXGeneration).Entity
		if dryRun {
			fmt.Fprintf(output, "%s: would-create with aes256k\n", entity)
			continue
		}
		if err := source.CreateKey(ctx, entity, caps, "aes256k"); err != nil {
			return cfg, err
		}
		fmt.Fprintf(output, "%s: created or already exists\n", entity)
	}
	if rotatable == 0 {
		return cfg, errors.New("no standard Rook CephX credentials are configured")
	}
	if dryRun {
		fmt.Fprintf(output, "Vault: would preserve generation %d and publish generation %d\n", cfg.CephXGeneration, next.CephXGeneration)
		return next, nil
	}

	if err := synchronize(ctx, archiveConfig(cfg), source, vault, syncOptions{Output: output}); err != nil {
		return cfg, fmt.Errorf("preserve CephX generation %d: %w", cfg.CephXGeneration, err)
	}
	if err := synchronize(ctx, archiveConfig(next), source, vault, syncOptions{Output: output}); err != nil {
		return cfg, fmt.Errorf("preserve CephX generation %d: %w", next.CephXGeneration, err)
	}
	if err := synchronize(ctx, next, source, vault, syncOptions{Output: output}); err != nil {
		return cfg, fmt.Errorf("publish CephX generation %d: %w", next.CephXGeneration, err)
	}
	return next, nil
}

func activeCredential(credential credentialSpec, generation int) credentialSpec {
	if generation == 0 {
		return credential
	}
	if _, ok := rookCephXProfile(credential.Entity, "default"); !ok {
		return credential
	}
	credential.Entity += "." + strconv.Itoa(generation)
	credential.UserID = strings.TrimPrefix(credential.Entity, "client.")
	return credential
}

func archiveConfig(cfg config) config {
	credentials := make([]credentialSpec, 0, len(cfg.Credentials))
	for _, credential := range cfg.Credentials {
		if _, ok := rookCephXProfile(credential.Entity, cfg.Ceph.RGWPoolPrefix); !ok {
			continue
		}
		credential.VaultPath = fmt.Sprintf("%s/generations/%d", credential.VaultPath, cfg.CephXGeneration)
		credentials = append(credentials, credential)
	}
	cfg.Credentials = credentials
	return cfg
}

func (version cephVersion) less(other cephVersion) bool {
	if version.Major != other.Major {
		return version.Major < other.Major
	}
	if version.Minor != other.Minor {
		return version.Minor < other.Minor
	}
	return version.Patch < other.Patch
}

func parseCephVersion(value string) (cephVersion, error) {
	fields := strings.Fields(value)
	if len(fields) < 3 || fields[0] != "ceph" || fields[1] != "version" {
		return cephVersion{}, fmt.Errorf("decode Ceph version: invalid response %q", strings.TrimSpace(value))
	}
	parts := strings.SplitN(fields[2], ".", 4)
	if len(parts) < 3 {
		return cephVersion{}, fmt.Errorf("decode Ceph version: invalid version %q", fields[2])
	}
	values := make([]int, 3)
	for i := range values {
		parsed, err := strconv.Atoi(parts[i])
		if err != nil {
			return cephVersion{}, fmt.Errorf("decode Ceph version: invalid version %q", fields[2])
		}
		values[i] = parsed
	}
	return cephVersion{Major: values[0], Minor: values[1], Patch: values[2]}, nil
}

func replaceConfig(name string, cfg config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	data = append(data, '\n')
	file, err := os.CreateTemp(filepath.Dir(name), ".pve-rook-secret-sync-*")
	if err != nil {
		return fmt.Errorf("create temporary config: %w", err)
	}
	temporary := file.Name()
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(temporary)
		}
	}()
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("write config: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync config: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close config: %w", err)
	}
	if err := os.Rename(temporary, name); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	remove = false
	return nil
}
