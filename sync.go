package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"
)

type cephReader interface {
	CanSynchronize(context.Context) (bool, string, error)
	FSID(context.Context) (string, error)
	Key(context.Context, string) (string, error)
}

type vaultStore interface {
	Read(context.Context, string) (vaultValue, error)
	Write(context.Context, string, map[string]string, int) error
}

type syncOptions struct {
	DryRun bool
	Check  bool
	Quiet  bool
	Output io.Writer
}

type desiredSecret struct {
	Path string
	Data map[string]string
}

type plannedSecret struct {
	Desired desiredSecret
	Current vaultValue
	Action  string
}

func synchronize(ctx context.Context, cfg config, source cephReader, vault vaultStore, options syncOptions) error {
	eligible, activeManager, err := source.CanSynchronize(ctx)
	if err != nil {
		return err
	}
	if !eligible {
		if !options.Quiet {
			fmt.Fprintf(options.Output, "standby: active ceph manager is %s\n", activeManager)
		}
		return nil
	}

	fsid, err := source.FSID(ctx)
	if err != nil {
		return err
	}

	keys := make(map[string]string, len(cfg.Credentials))
	for _, credential := range cfg.Credentials {
		key, err := source.Key(ctx, credential.Entity)
		if err != nil {
			return err
		}
		keys[credential.Entity] = key
	}

	desired, err := buildDesired(cfg, fsid, keys)
	if err != nil {
		return err
	}
	plan := make([]plannedSecret, 0, len(desired))
	drift := false
	for _, secret := range desired {
		current, err := vault.Read(ctx, secret.Path)
		if err != nil {
			return err
		}
		if currentFSID := storedFSID(current.Data); currentFSID != "" && currentFSID != fsid {
			return fmt.Errorf("vault path %s belongs to ceph cluster %s, not %s", secret.Path, currentFSID, fsid)
		}
		action := "unchanged"
		if !current.Exists {
			action = "create"
			drift = true
		} else if !sameCredentialData(current.Data, secret.Data) {
			action = "update"
			drift = true
		}
		plan = append(plan, plannedSecret{Desired: secret, Current: current, Action: action})
	}

	for _, item := range plan {
		if options.Quiet && item.Action == "unchanged" {
			continue
		}
		if options.DryRun && item.Action != "unchanged" {
			fmt.Fprintf(options.Output, "%s: would-%s\n", item.Desired.Path, item.Action)
		} else {
			fmt.Fprintf(options.Output, "%s: %s\n", item.Desired.Path, item.Action)
		}
	}
	if options.Check && drift {
		return errDrift
	}
	if options.DryRun || !drift {
		return nil
	}

	syncedAt := time.Now().UTC().Format(time.RFC3339)
	for _, item := range plan {
		if item.Action == "unchanged" {
			continue
		}
		data := maps.Clone(item.Desired.Data)
		data["_synced_at"] = syncedAt
		if err := vault.Write(ctx, item.Desired.Path, data, item.Current.Version); err != nil {
			return err
		}
		fmt.Fprintf(options.Output, "%s: written\n", item.Desired.Path)
	}
	return nil
}

func buildDesired(cfg config, fsid string, keys map[string]string) ([]desiredSecret, error) {
	generation, err := credentialGeneration(fsid, cfg, keys)
	if err != nil {
		return nil, err
	}

	result := make([]desiredSecret, 0, len(cfg.Credentials))
	for _, credential := range cfg.Credentials {
		key, ok := keys[credential.Entity]
		if !ok || key == "" {
			return nil, fmt.Errorf("missing key for %s", credential.Entity)
		}
		userID := credential.UserID
		if userID == "" {
			userID = strings.TrimPrefix(credential.Entity, "client.")
		}
		data := map[string]string{
			"_ceph_entity":     credential.Entity,
			"_ceph_fsid":       fsid,
			"_sync_generation": generation,
		}
		switch credential.Kind {
		case "rook-mon":
			data["cluster-name"] = cfg.RookClusterName
			data["fsid"] = fsid
			data["admin-secret"] = "admin-secret"
			data["mon-secret"] = "mon-secret"
			data["ceph-username"] = credential.Entity
			data["ceph-secret"] = key
		case "rook-csi":
			data["userID"] = userID
			data["userKey"] = key
		case "rook-cephfs-csi":
			data["userID"] = userID
			data["userKey"] = key
			data["adminID"] = userID
			data["adminKey"] = key
		}
		result = append(result, desiredSecret{Path: credential.VaultPath, Data: data})
	}
	return result, nil
}

func credentialGeneration(fsid string, cfg config, keys map[string]string) (string, error) {
	parts := make([]string, 0, len(cfg.Credentials))
	for _, credential := range cfg.Credentials {
		key, ok := keys[credential.Entity]
		if !ok || key == "" {
			return "", fmt.Errorf("missing key for %s", credential.Entity)
		}
		identity := credential.UserID
		if identity == "" {
			identity = strings.TrimPrefix(credential.Entity, "client.")
		}
		if credential.Kind == "rook-mon" {
			identity = cfg.RookClusterName
		}
		parts = append(parts, strings.Join([]string{credential.VaultPath, credential.Entity, credential.Kind, identity, key}, "\x00"))
	}
	slices.Sort(parts)
	hash := sha256.New()
	_, _ = io.WriteString(hash, fsid+"\x00")
	for _, part := range parts {
		_, _ = io.WriteString(hash, part+"\x00")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func storedFSID(data map[string]string) string {
	if fsid := data["_ceph_fsid"]; fsid != "" {
		return fsid
	}
	return data["fsid"]
}

func sameCredentialData(current, desired map[string]string) bool {
	current = maps.Clone(current)
	delete(current, "_synced_at")
	return maps.Equal(current, desired)
}
