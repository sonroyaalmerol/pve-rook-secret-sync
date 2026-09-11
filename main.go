package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"
)

var errDrift = errors.New("credentials differ from Vault")

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	trigger := make(chan os.Signal, 1)
	signal.Notify(trigger, syscall.SIGHUP, syscall.SIGUSR1)
	defer signal.Stop(trigger)
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr, trigger))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, trigger <-chan os.Signal) int {
	if len(args) == 0 || args[0] != "sync" {
		printUsage(stderr)
		return 2
	}

	flags := flag.NewFlagSet("sync", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "", "path to JSON configuration")
	dryRun := flags.Bool("dry-run", false, "show changes without writing to Vault")
	check := flags.Bool("check", false, "exit 2 when Vault differs without writing")
	timeout := flags.Duration("timeout", 30*time.Second, "overall operation timeout")
	interval := flags.Duration("interval", 0, "repeat synchronization at this interval")
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "unexpected argument %q\n", flags.Arg(0))
		return 2
	}
	if *configPath == "" {
		fmt.Fprintln(stderr, "-config is required")
		return 2
	}
	if *dryRun && *check {
		fmt.Fprintln(stderr, "-dry-run and -check cannot be combined")
		return 2
	}
	if *timeout <= 0 {
		fmt.Fprintln(stderr, "-timeout must be positive")
		return 2
	}
	if *interval < 0 {
		fmt.Fprintln(stderr, "-interval must not be negative")
		return 2
	}

	syncOnce := func(ctx context.Context) error {
		cfg, err := loadConfig(*configPath)
		if err != nil {
			return err
		}
		client, err := newVaultClient(cfg.Vault)
		if err != nil {
			return err
		}
		defer client.http.CloseIdleConnections()

		ctx, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		return synchronize(ctx, cfg, newCephSource(cfg.Ceph), client, syncOptions{
			DryRun: *dryRun,
			Check:  *check,
			Quiet:  *interval > 0,
			Output: stdout,
		})
	}

	if *interval > 0 {
		watch(ctx, *interval, trigger, syncOnce, stderr)
		return 0
	}
	err := syncOnce(ctx)
	if errors.Is(err, errDrift) {
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func watch(ctx context.Context, interval time.Duration, trigger <-chan os.Signal, syncOnce func(context.Context) error, stderr io.Writer) {
	for {
		if err := syncOnce(ctx); err != nil && ctx.Err() == nil {
			fmt.Fprintln(stderr, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		case <-trigger:
		}
	}
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: ceph-vault-sync sync -config FILE [-dry-run|-check] [-timeout DURATION] [-interval DURATION]")
}
