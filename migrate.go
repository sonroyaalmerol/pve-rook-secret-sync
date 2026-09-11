package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"
)

const secureKeyType = "aes256k"

type authKeyState struct {
	Current string
	Pending string
}

type migrationSource interface {
	rotationSource
	RunMigrationHelper(context.Context, bool) ([]byte, error)
	AuthKeyStates(context.Context) (map[string]authKeyState, error)
	StagePendingKey(context.Context, string) (string, error)
	InstallRGWKey(context.Context, rgwDaemonConfig, string) error
	RestartRGW(context.Context, rgwDaemonConfig) error
}

func runMigrate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("migrate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "", "path to JSON configuration")
	dryRun := flags.Bool("dry-run", false, "show the migration without changing keys, keyrings, services, or Vault")
	timeout := flags.Duration("timeout", time.Hour, "overall operation timeout")
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
	client, err := newVaultClient(ctx, cfg.Vault, newVaultAuth())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer client.http.CloseIdleConnections()

	next, err := migrateToSecureKeys(ctx, cfg, newCephSource(cfg.Ceph), client, *dryRun, stdout)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if !*dryRun && next.CephXGeneration != cfg.CephXGeneration {
		if err := replaceConfig(path, next); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintf(stdout, "%s: active CephX generation is %d\n", path, next.CephXGeneration)
	}
	return 0
}

func migrateToSecureKeys(ctx context.Context, cfg config, source migrationSource, vault vaultStore, dryRun bool, output io.Writer) (config, error) {
	eligible, activeManager, err := source.CanSynchronize(ctx)
	if err != nil {
		return cfg, err
	}
	if !eligible {
		return cfg, fmt.Errorf("migration must run on active ceph manager %s", activeManager)
	}
	version, err := source.Version(ctx)
	if err != nil {
		return cfg, err
	}
	if version.less(cephVersion{Major: 19, Minor: 2, Patch: 6}) {
		return cfg, fmt.Errorf("AES256K migration requires Ceph 19.2.6 or newer; found %d.%d.%d", version.Major, version.Minor, version.Patch)
	}

	helperOutput, err := source.RunMigrationHelper(ctx, !dryRun)
	if err != nil {
		return cfg, err
	}
	if _, err := output.Write(helperOutput); err != nil {
		return cfg, fmt.Errorf("write migration helper output: %w", err)
	}

	states, err := source.AuthKeyStates(ctx)
	if err != nil {
		return cfg, err
	}
	for _, daemon := range cfg.Ceph.RGWDaemons {
		if err := migrateRGWDaemon(ctx, source, daemon, states[daemon.Entity], dryRun, output); err != nil {
			return cfg, err
		}
	}

	rotate, err := rookNeedsSecureKeys(cfg, states)
	if err != nil {
		return cfg, err
	}
	if !rotate {
		fmt.Fprintf(output, "Rook: generation %d already uses %s\n", cfg.CephXGeneration, secureKeyType)
		return cfg, nil
	}
	return rotateCredentials(ctx, cfg, source, vault, dryRun, output)
}

func migrateRGWDaemon(ctx context.Context, source migrationSource, daemon rgwDaemonConfig, state authKeyState, dryRun bool, output io.Writer) error {
	switch {
	case state.Current == secureKeyType:
		fmt.Fprintf(output, "%s: already uses %s\n", daemon.Entity, secureKeyType)
		return nil
	case state.Current == "":
		return fmt.Errorf("RGW entity %s is missing from the CephX key inventory", daemon.Entity)
	case state.Pending != "" && state.Pending != secureKeyType:
		return fmt.Errorf("RGW entity %s already has a pending %s key; resolve it with ceph auth clear-pending", daemon.Entity, state.Pending)
	}
	if dryRun {
		fmt.Fprintf(output, "%s: would stage %s, write %s on %s, and restart %s\n", daemon.Entity, secureKeyType, daemon.Keyring, daemon.Host, daemon.Unit)
		return nil
	}

	key, err := source.StagePendingKey(ctx, daemon.Entity)
	if err != nil {
		return err
	}
	if err := source.InstallRGWKey(ctx, daemon, key); err != nil {
		return err
	}
	if err := source.RestartRGW(ctx, daemon); err != nil {
		return err
	}
	promoted, err := awaitKeyPromotion(ctx, source, daemon.Entity)
	if err != nil {
		return err
	}
	if !promoted {
		return fmt.Errorf("%s did not authenticate with its new key; its current key still works, so check %s on %s", daemon.Entity, daemon.Unit, daemon.Host)
	}
	fmt.Fprintf(output, "%s: migrated to %s on %s\n", daemon.Entity, secureKeyType, daemon.Host)
	return nil
}

func awaitKeyPromotion(ctx context.Context, source migrationSource, entity string) (bool, error) {
	for attempt := range 10 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return false, ctx.Err()
			case <-time.After(3 * time.Second):
			}
		}
		states, err := source.AuthKeyStates(ctx)
		if err != nil {
			return false, err
		}
		if states[entity].Current == secureKeyType {
			return true, nil
		}
	}
	return false, nil
}

func rookNeedsSecureKeys(cfg config, states map[string]authKeyState) (bool, error) {
	configured := false
	for _, credential := range cfg.Credentials {
		if _, ok := rookCephXProfile(credential.Entity, cfg.Ceph.RGWPoolPrefix); !ok {
			continue
		}
		configured = true
		entity := activeCredential(credential, cfg.CephXGeneration).Entity
		if states[entity].Current != secureKeyType {
			return true, nil
		}
	}
	if !configured {
		return false, errors.New("no standard Rook CephX credentials are configured")
	}
	return false, nil
}
