package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"
)

type bootstrapSource interface {
	Key(context.Context, string) (string, error)
	CreateKey(context.Context, string, []string, string) error
	RGWCredentials(context.Context, string) (map[string]string, error)
	EnsureRGWAdmin(context.Context, string) (bool, error)
}

func runBootstrap(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "", "path to JSON configuration")
	dryRun := flags.Bool("dry-run", false, "show missing users without creating them")
	timeout := flags.Duration("timeout", 30*time.Second, "overall operation timeout")
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
	if err := bootstrap(ctx, cfg, newCephSource(cfg.Ceph), *dryRun, stdout); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func bootstrap(ctx context.Context, cfg config, source bootstrapSource, dryRun bool, output io.Writer) error {
	seen := make(map[string]struct{}, len(cfg.Credentials))
	for _, credential := range cfg.Credentials {
		switch credential.Kind {
		case "rook-mon", "rook-csi", "rook-cephfs-csi":
			if _, ok := seen[credential.Entity]; ok {
				continue
			}
			seen[credential.Entity] = struct{}{}
			caps, ok := rookCephXProfile(credential.Entity, cfg.Ceph.RGWPoolPrefix)
			if !ok {
				return fmt.Errorf("cannot bootstrap non-standard CephX entity %q; create it manually", credential.Entity)
			}
			if _, err := source.Key(ctx, credential.Entity); err == nil {
				fmt.Fprintf(output, "%s: exists\n", credential.Entity)
				continue
			}
			if dryRun {
				fmt.Fprintf(output, "%s: would-create\n", credential.Entity)
				continue
			}
			if err := source.CreateKey(ctx, credential.Entity, caps, ""); err != nil {
				return err
			}
			fmt.Fprintf(output, "%s: created\n", credential.Entity)
		case "rook-rgw-admin":
			if _, ok := seen[credential.Entity]; ok {
				continue
			}
			seen[credential.Entity] = struct{}{}
			if dryRun {
				if _, err := source.RGWCredentials(ctx, credential.Entity); err == nil {
					fmt.Fprintf(output, "%s: exists\n", credential.Entity)
				} else {
					fmt.Fprintf(output, "%s: would-create\n", credential.Entity)
				}
				continue
			}
			created, err := source.EnsureRGWAdmin(ctx, credential.Entity)
			if err != nil {
				return err
			}
			if created {
				fmt.Fprintf(output, "%s: created\n", credential.Entity)
			} else {
				fmt.Fprintf(output, "%s: exists\n", credential.Entity)
			}
		}
	}
	return nil
}

func rookCephXProfile(entity, rgwPoolPrefix string) ([]string, bool) {
	switch entity {
	case "client.healthchecker":
		return []string{
			"mon", "allow r, allow command quorum_status, allow command version",
			"mgr", "allow command config",
			"osd", fmt.Sprintf("profile rbd-read-only, allow rwx pool=%s.rgw.meta, allow r pool=.rgw.root, allow rw pool=%s.rgw.control, allow rx pool=%s.rgw.log, allow x pool=%s.rgw.buckets.index", rgwPoolPrefix, rgwPoolPrefix, rgwPoolPrefix, rgwPoolPrefix),
			"mds", "allow *",
		}, true
	case "client.csi-rbd-node":
		return []string{"mon", "profile rbd, allow command 'osd blocklist'", "osd", "profile rbd"}, true
	case "client.csi-rbd-provisioner":
		return []string{"mon", "profile rbd, allow command 'osd blocklist'", "mgr", "allow rw", "osd", "profile rbd"}, true
	case "client.csi-cephfs-node":
		return []string{"mon", "allow r, allow command 'osd blocklist'", "mgr", "allow rw", "osd", "allow rw tag cephfs *=*", "mds", "allow rw"}, true
	case "client.csi-cephfs-provisioner":
		return []string{"mon", "allow r, allow command 'osd blocklist'", "mgr", "allow rw", "osd", "allow rw tag cephfs metadata=*", "mds", "allow *"}, true
	default:
		return nil, false
	}
}
