package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

type fakeRotationSource struct {
	fakeCeph
	version  cephVersion
	created  []string
	keyTypes []string
}

func (source *fakeRotationSource) CreateKey(_ context.Context, entity string, _ []string, keyType string) error {
	source.created = append(source.created, entity)
	source.keyTypes = append(source.keyTypes, keyType)
	source.keys[entity] = "key-" + entity
	return nil
}

func (source *fakeRotationSource) Version(context.Context) (cephVersion, error) {
	return source.version, nil
}

func TestRotateCredentialsPublishesAndPreservesGenerations(t *testing.T) {
	cfg := testConfig()
	source := &fakeRotationSource{
		fakeCeph: fakeCeph{
			fsid: "cluster-a",
			keys: map[string]string{
				"client.healthchecker": "old-mon",
				"client.csi-rbd-node":  "old-rbd",
			},
		},
		version: cephVersion{Major: 19, Minor: 2, Patch: 6},
	}
	vault := &fakeVault{values: map[string]vaultValue{}, writes: map[string]map[string]string{}}
	var output bytes.Buffer

	next, err := rotateCredentials(context.Background(), cfg, source, vault, false, &output)
	if err != nil {
		t.Fatal(err)
	}
	if next.CephXGeneration != 1 {
		t.Fatalf("generation = %d, want 1", next.CephXGeneration)
	}
	if got := strings.Join(source.created, ","); got != "client.healthchecker.1,client.csi-rbd-node.1" {
		t.Fatalf("created = %q", got)
	}
	for _, keyType := range source.keyTypes {
		if keyType != "aes256k" {
			t.Fatalf("key type = %q, want aes256k", keyType)
		}
	}
	for _, path := range []string{
		"rook-ceph-mon/generations/0",
		"rook-csi-rbd-node/generations/0",
		"rook-ceph-mon/generations/1",
		"rook-csi-rbd-node/generations/1",
		"rook-ceph-mon",
		"rook-csi-rbd-node",
	} {
		if vault.writes[path] == nil {
			t.Errorf("missing Vault write %q", path)
		}
	}
	if got := vault.writes["rook-csi-rbd-node"]["userID"]; got != "csi-rbd-node.1" {
		t.Fatalf("active userID = %q", got)
	}
}

func TestRotateCredentialsRejectsOldCeph(t *testing.T) {
	cfg := testConfig()
	source := &fakeRotationSource{
		fakeCeph: fakeCeph{fsid: "cluster-a", keys: map[string]string{}},
		version:  cephVersion{Major: 19, Minor: 2, Patch: 5},
	}
	vault := &fakeVault{values: map[string]vaultValue{}, writes: map[string]map[string]string{}}

	_, err := rotateCredentials(context.Background(), cfg, source, vault, true, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "19.2.6") {
		t.Fatalf("error = %v", err)
	}
}

func TestParseCephVersion(t *testing.T) {
	version, err := parseCephVersion("ceph version 19.2.6 (hash) squid (stable)")
	if err != nil {
		t.Fatal(err)
	}
	if version != (cephVersion{Major: 19, Minor: 2, Patch: 6}) {
		t.Fatalf("version = %+v", version)
	}
	if _, err := parseCephVersion("19.2.6"); err == nil {
		t.Fatal("invalid version was accepted")
	}
}

func TestActiveCredential(t *testing.T) {
	credential := activeCredential(credentialSpec{Entity: "client.csi-rbd-node", Kind: "rook-csi"}, 3)
	if credential.Entity != "client.csi-rbd-node.3" || credential.UserID != "csi-rbd-node.3" {
		t.Fatalf("credential = %+v", credential)
	}
}
