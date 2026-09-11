package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

type fakeCeph struct {
	fsid    string
	keys    map[string]string
	values  map[string]map[string]string
	standby bool
}

func (ceph fakeCeph) CanSynchronize(context.Context) (bool, string, error) {
	return !ceph.standby, "pve1", nil
}

func (ceph fakeCeph) FSID(context.Context) (string, error) {
	return ceph.fsid, nil
}

func (ceph fakeCeph) Credential(_ context.Context, credential credentialSpec) (map[string]string, error) {
	if ceph.values != nil {
		return ceph.values[credential.VaultPath], nil
	}
	return map[string]string{"key": ceph.keys[credential.Entity]}, nil
}

type fakeVault struct {
	values map[string]vaultValue
	writes map[string]map[string]string
	reads  int
}

func (vault *fakeVault) Read(_ context.Context, path string) (vaultValue, error) {
	vault.reads++
	return vault.values[path], nil
}

func (vault *fakeVault) Write(_ context.Context, path string, data map[string]string, _ int) error {
	vault.writes[path] = data
	return nil
}

func TestBuildDesired(t *testing.T) {
	cfg := testConfig()
	cfg.Credentials = append(cfg.Credentials,
		credentialSpec{VaultPath: "rook-ceph-config", Kind: "rook-config"},
		credentialSpec{VaultPath: "rook-ceph-dashboard-link", Kind: "rook-dashboard"},
		credentialSpec{VaultPath: "rgw-admin-ops-user", Entity: "rgw-admin-ops-user", Kind: "rook-rgw-admin"},
	)
	values := map[string]map[string]string{
		"rook-ceph-mon":            {"key": "mon-key"},
		"rook-csi-rbd-node":        {"key": "rbd-key"},
		"rook-ceph-config":         {"mon_host": "[v2:10.0.0.1:3300]", "mon_initial_members": "pve1"},
		"rook-ceph-dashboard-link": {"url": "https://10.0.0.1:8443/"},
		"rgw-admin-ops-user":       {"accessKey": "access", "secretKey": "secret"},
	}
	secrets, err := buildDesired(cfg, "fsid", values)
	if err != nil {
		t.Fatal(err)
	}
	if len(secrets) != 5 {
		t.Fatalf("got %d secrets, want 5", len(secrets))
	}
	mon := secrets[0].Data
	if mon["ceph-username"] != "client.healthchecker" || mon["ceph-secret"] != "mon-key" || mon["fsid"] != "fsid" {
		t.Fatalf("unexpected monitor secret: %+v", mon)
	}
	rbd := secrets[1].Data
	if rbd["userID"] != "csi-rbd-node" || rbd["userKey"] != "rbd-key" {
		t.Fatalf("unexpected RBD secret: %+v", rbd)
	}
	if config := secrets[2].Data; config["mon_host"] != "[v2:10.0.0.1:3300]" || config["mon_initial_members"] != "pve1" {
		t.Fatalf("unexpected config secret: %+v", config)
	}
	if dashboard := secrets[3].Data; dashboard["userID"] != "ceph-dashboard-link" || dashboard["userKey"] != "https://10.0.0.1:8443/" {
		t.Fatalf("unexpected dashboard secret: %+v", dashboard)
	}
	if rgw := secrets[4].Data; rgw["accessKey"] != "access" || rgw["secretKey"] != "secret" {
		t.Fatalf("unexpected RGW secret: %+v", rgw)
	}
	if mon["_sync_generation"] == "" || mon["_sync_generation"] != rbd["_sync_generation"] {
		t.Fatal("secrets do not share a generation")
	}
}

func TestCredentialGenerationTracksRenderedData(t *testing.T) {
	values := map[string]map[string]string{
		"rook-ceph-mon":     {"key": "mon-key"},
		"rook-csi-rbd-node": {"key": "rbd-key"},
	}
	tests := []struct {
		name   string
		change func(*config)
	}{
		{"cluster name", func(cfg *config) { cfg.RookClusterName = "other-cluster" }},
		{"credential kind", func(cfg *config) { cfg.Credentials[1].Kind = "rook-cephfs-csi" }},
		{"user ID", func(cfg *config) { cfg.Credentials[1].UserID = "other-user" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := testConfig()
			before, err := credentialGeneration("fsid", cfg, values)
			if err != nil {
				t.Fatal(err)
			}
			test.change(&cfg)
			after, err := credentialGeneration("fsid", cfg, values)
			if err != nil {
				t.Fatal(err)
			}
			if before == after {
				t.Fatal("generation did not change")
			}
		})
	}
}

func TestSynchronizeCheckAndWrite(t *testing.T) {
	cfg := testConfig()
	ceph := fakeCeph{fsid: "fsid", keys: map[string]string{"client.healthchecker": "mon-key", "client.csi-rbd-node": "rbd-key"}}
	vault := &fakeVault{values: map[string]vaultValue{}, writes: map[string]map[string]string{}}

	err := synchronize(context.Background(), cfg, ceph, vault, syncOptions{Check: true, Output: io.Discard})
	if !errors.Is(err, errDrift) || len(vault.writes) != 0 {
		t.Fatalf("check got error %v and %d writes", err, len(vault.writes))
	}

	err = synchronize(context.Background(), cfg, ceph, vault, syncOptions{Output: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if len(vault.writes) != 2 {
		t.Fatalf("got %d writes, want 2", len(vault.writes))
	}
	generation := vault.writes["rook-ceph-mon"]["_sync_generation"]
	if generation == "" || generation != vault.writes["rook-csi-rbd-node"]["_sync_generation"] {
		t.Fatal("writes do not share a generation")
	}
	if vault.writes["rook-ceph-mon"]["_synced_at"] == "" {
		t.Fatal("write has no synchronization timestamp")
	}
}

func TestSynchronizeStandbyDoesNotAccessVault(t *testing.T) {
	var output bytes.Buffer
	ceph := fakeCeph{standby: true}
	vault := &fakeVault{values: map[string]vaultValue{}, writes: map[string]map[string]string{}}

	if err := synchronize(context.Background(), testConfig(), ceph, vault, syncOptions{Output: &output}); err != nil {
		t.Fatal(err)
	}
	if vault.reads != 0 || len(vault.writes) != 0 {
		t.Fatalf("standby performed %d reads and %d writes", vault.reads, len(vault.writes))
	}
	if output.String() != "standby: active ceph manager is pve1\n" {
		t.Fatalf("unexpected output %q", output.String())
	}
	output.Reset()
	if err := synchronize(context.Background(), testConfig(), ceph, vault, syncOptions{Quiet: true, Output: &output}); err != nil {
		t.Fatal(err)
	}
	if output.Len() != 0 {
		t.Fatalf("quiet standby output %q", output.String())
	}
}

func TestSynchronizeRejectsAnotherCluster(t *testing.T) {
	cfg := testConfig()
	ceph := fakeCeph{fsid: "fsid", keys: map[string]string{"client.healthchecker": "mon-key", "client.csi-rbd-node": "rbd-key"}}
	vault := &fakeVault{
		values: map[string]vaultValue{"rook-ceph-mon": {Exists: true, Data: map[string]string{"_ceph_fsid": "other-fsid"}}},
		writes: map[string]map[string]string{},
	}

	err := synchronize(context.Background(), cfg, ceph, vault, syncOptions{Output: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "belongs to ceph cluster other-fsid") {
		t.Fatalf("got %v, want cluster ownership error", err)
	}
	if len(vault.writes) != 0 {
		t.Fatalf("wrote %d secrets for another cluster", len(vault.writes))
	}
}

func testConfig() config {
	return config{
		RookClusterName: "rook-ceph",
		Credentials: []credentialSpec{
			{VaultPath: "rook-ceph-mon", Entity: "client.healthchecker", Kind: "rook-mon"},
			{VaultPath: "rook-csi-rbd-node", Entity: "client.csi-rbd-node", Kind: "rook-csi"},
		},
	}
}
