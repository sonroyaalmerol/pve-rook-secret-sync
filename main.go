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
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
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

	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	client, err := newVaultClient(cfg.Vault)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	err = synchronize(ctx, cfg, newCephSource(cfg.Ceph), client, syncOptions{
		DryRun: *dryRun,
		Check:  *check,
		Output: stdout,
	})
	if errors.Is(err, errDrift) {
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: ceph-vault-sync sync -config FILE [-dry-run|-check] [-timeout DURATION]")
}
