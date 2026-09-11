package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"
)

type provisionSource interface {
	bootstrapSource
	cephReader
	InitRBDPool(context.Context, string) error
	EnsureCephFSSubvolumeGroup(context.Context, string) error
}

type provisionOptions struct {
	RBDPool    string
	CephFSName string
	DryRun     bool
	Output     io.Writer
}

func runProvision(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("provision", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "", "path to JSON configuration")
	rbdPool := flags.String("rbd-pool", "", "existing Ceph pool to initialize for RBD")
	cephFSName := flags.String("cephfs-name", "", "existing CephFS filesystem to initialize for CSI")
	dryRun := flags.Bool("dry-run", false, "show provider and Vault changes")
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
	if *rbdPool == "" && *cephFSName == "" {
		fmt.Fprintln(stderr, "-rbd-pool or -cephfs-name is required")
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
	if err := provisionProvider(ctx, cfg, newCephSource(cfg.Ceph), client, provisionOptions{
		RBDPool:    *rbdPool,
		CephFSName: *cephFSName,
		DryRun:     *dryRun,
		Output:     stdout,
	}); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func provisionProvider(ctx context.Context, cfg config, source provisionSource, vault vaultStore, options provisionOptions) error {
	eligible, activeManager, err := source.CanSynchronize(ctx)
	if err != nil {
		return err
	}
	if !eligible {
		return fmt.Errorf("provisioning must run on active ceph manager %s", activeManager)
	}
	if options.RBDPool != "" {
		if options.DryRun {
			fmt.Fprintf(options.Output, "%s: would-initialize RBD pool\n", options.RBDPool)
		} else {
			if err := source.InitRBDPool(ctx, options.RBDPool); err != nil {
				return err
			}
			fmt.Fprintf(options.Output, "%s: initialized RBD pool\n", options.RBDPool)
		}
	}
	if options.CephFSName != "" {
		if options.DryRun {
			fmt.Fprintf(options.Output, "%s/csi: would-create and pin CephFS subvolume group\n", options.CephFSName)
		} else {
			if err := source.EnsureCephFSSubvolumeGroup(ctx, options.CephFSName); err != nil {
				return err
			}
			fmt.Fprintf(options.Output, "%s/csi: created and pinned CephFS subvolume group\n", options.CephFSName)
		}
	}
	if err := bootstrap(ctx, cfg, source, options.DryRun, options.Output); err != nil {
		return err
	}
	return synchronize(ctx, cfg, source, vault, syncOptions{DryRun: options.DryRun, Output: options.Output})
}
