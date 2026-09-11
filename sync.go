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
	Credential(context.Context, credentialSpec) (map[string]string, error)
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

	values := make(map[string]map[string]string, len(cfg.Credentials))
	for _, credential := range cfg.Credentials {
		credential = activeCredential(credential, cfg.CephXGeneration)
		value, err := source.Credential(ctx, credential)
		if err != nil {
			return err
		}
		values[credential.VaultPath] = value
	}

	desired, err := buildDesired(cfg, fsid, values)
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

func buildDesired(cfg config, fsid string, values map[string]map[string]string) ([]desiredSecret, error) {
	generation, err := credentialGeneration(fsid, cfg, values)
	if err != nil {
		return nil, err
	}

	result := make([]desiredSecret, 0, len(cfg.Credentials))
	for _, credential := range cfg.Credentials {
		credential = activeCredential(credential, cfg.CephXGeneration)
		data, err := renderCredential(cfg, fsid, credential, values[credential.VaultPath])
		if err != nil {
			return nil, err
		}
		data["_sync_generation"] = generation
		result = append(result, desiredSecret{Path: credential.VaultPath, Data: data})
	}
	return result, nil
}

func renderCredential(cfg config, fsid string, credential credentialSpec, value map[string]string) (map[string]string, error) {
	data := map[string]string{"_ceph_fsid": fsid}
	if credential.Entity != "" {
		data["_ceph_entity"] = credential.Entity
	}
	userID := credential.UserID
	if userID == "" {
		userID = strings.TrimPrefix(credential.Entity, "client.")
	}
	require := func(name string) (string, error) {
		if value[name] == "" {
			return "", fmt.Errorf("missing %s for %s", name, credential.VaultPath)
		}
		return value[name], nil
	}
	switch credential.Kind {
	case "rook-mon":
		key, err := require("key")
		if err != nil {
			return nil, err
		}
		data["cluster-name"] = cfg.RookClusterName
		data["fsid"] = fsid
		data["admin-secret"] = "admin-secret"
		data["mon-secret"] = "mon-secret"
		data["ceph-username"] = credential.Entity
		data["ceph-secret"] = key
	case "rook-csi":
		key, err := require("key")
		if err != nil {
			return nil, err
		}
		data["userID"] = userID
		data["userKey"] = key
	case "rook-cephfs-csi":
		key, err := require("key")
		if err != nil {
			return nil, err
		}
		data["userID"] = userID
		data["userKey"] = key
		data["adminID"] = userID
		data["adminKey"] = key
	case "rook-config":
		monHost, err := require("mon_host")
		if err != nil {
			return nil, err
		}
		members, err := require("mon_initial_members")
		if err != nil {
			return nil, err
		}
		data["mon_host"] = monHost
		data["mon_initial_members"] = members
	case "rook-dashboard":
		link, err := require("url")
		if err != nil {
			return nil, err
		}
		data["userID"] = "ceph-dashboard-link"
		data["userKey"] = link
	case "rook-rgw-admin":
		accessKey, err := require("accessKey")
		if err != nil {
			return nil, err
		}
		secretKey, err := require("secretKey")
		if err != nil {
			return nil, err
		}
		data["accessKey"] = accessKey
		data["secretKey"] = secretKey
	default:
		return nil, fmt.Errorf("unsupported credential kind %q", credential.Kind)
	}
	return data, nil
}

func credentialGeneration(fsid string, cfg config, values map[string]map[string]string) (string, error) {
	parts := make([]string, 0, len(cfg.Credentials))
	for _, credential := range cfg.Credentials {
		credential = activeCredential(credential, cfg.CephXGeneration)
		data, err := renderCredential(cfg, fsid, credential, values[credential.VaultPath])
		if err != nil {
			return "", err
		}
		fields := make([]string, 0, len(data))
		for key, value := range data {
			fields = append(fields, key+"\x00"+value)
		}
		slices.Sort(fields)
		parts = append(parts, credential.VaultPath+"\x00"+strings.Join(fields, "\x00"))
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
