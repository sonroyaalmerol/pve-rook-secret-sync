package main

import (
	"context"
	"errors"
	"io"
	"testing"
)

type fakeCeph struct {
	fsid string
	keys map[string]string
}

func (ceph fakeCeph) FSID(context.Context) (string, error) {
	return ceph.fsid, nil
}

func (ceph fakeCeph) Key(_ context.Context, entity string) (string, error) {
	return ceph.keys[entity], nil
}

type fakeVault struct {
	values map[string]vaultValue
	writes map[string]map[string]string
}

func (vault *fakeVault) Read(_ context.Context, path string) (vaultValue, error) {
	return vault.values[path], nil
}

func (vault *fakeVault) Write(_ context.Context, path string, data map[string]string, _ int) error {
	vault.writes[path] = data
	return nil
}

func TestBuildDesired(t *testing.T) {
	cfg := testConfig()
	keys := map[string]string{"client.healthchecker": "mon-key", "client.csi-rbd-node": "rbd-key"}
	secrets, err := buildDesired(cfg, "fsid", keys)
	if err != nil {
		t.Fatal(err)
	}
	if len(secrets) != 2 {
		t.Fatalf("got %d secrets, want 2", len(secrets))
	}
	mon := secrets[0].Data
	if mon["ceph-username"] != "client.healthchecker" || mon["ceph-secret"] != "mon-key" || mon["fsid"] != "fsid" {
		t.Fatalf("unexpected monitor secret: %+v", mon)
	}
	rbd := secrets[1].Data
	if rbd["userID"] != "csi-rbd-node" || rbd["userKey"] != "rbd-key" {
		t.Fatalf("unexpected RBD secret: %+v", rbd)
	}
	if mon["_sync_generation"] == "" || mon["_sync_generation"] != rbd["_sync_generation"] {
		t.Fatal("secrets do not share a generation")
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

func testConfig() config {
	return config{
		RookClusterName: "rook-ceph",
		Credentials: []credentialSpec{
			{VaultPath: "rook-ceph-mon", Entity: "client.healthchecker", Kind: "rook-mon"},
			{VaultPath: "rook-csi-rbd-node", Entity: "client.csi-rbd-node", Kind: "rook-csi"},
		},
	}
}
