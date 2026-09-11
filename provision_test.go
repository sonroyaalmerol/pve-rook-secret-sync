package main

import (
	"bytes"
	"context"
	"testing"
)

type fakeProvisionSource struct {
	*fakeRotationSource
	rbdPools    []string
	filesystems []string
}

func (source *fakeProvisionSource) Key(_ context.Context, entity string) (string, error) {
	return source.keys[entity], nil
}

func (source *fakeProvisionSource) RGWCredentials(context.Context, string) (map[string]string, error) {
	return nil, nil
}

func (source *fakeProvisionSource) EnsureRGWAdmin(context.Context, string) (bool, error) {
	return false, nil
}

func (source *fakeProvisionSource) InitRBDPool(_ context.Context, pool string) error {
	source.rbdPools = append(source.rbdPools, pool)
	return nil
}

func (source *fakeProvisionSource) EnsureCephFSSubvolumeGroup(_ context.Context, filesystem string) error {
	source.filesystems = append(source.filesystems, filesystem)
	return nil
}

func TestProvisionProviderInitializesStorageAndSynchronizes(t *testing.T) {
	cfg := testConfig()
	source := &fakeProvisionSource{fakeRotationSource: &fakeRotationSource{fakeCeph: fakeCeph{
		fsid: "cluster-a",
		keys: map[string]string{
			"client.healthchecker": "mon-key",
			"client.csi-rbd-node":  "rbd-key",
		},
	}}}
	vault := &fakeVault{values: map[string]vaultValue{}, writes: map[string]map[string]string{}}
	var output bytes.Buffer

	if err := provisionProvider(context.Background(), cfg, source, vault, provisionOptions{
		RBDPool:    "kubernetes",
		CephFSName: "phoenix",
		Output:     &output,
	}); err != nil {
		t.Fatal(err)
	}
	if len(source.rbdPools) != 1 || source.rbdPools[0] != "kubernetes" {
		t.Fatalf("RBD pools = %v", source.rbdPools)
	}
	if len(source.filesystems) != 1 || source.filesystems[0] != "phoenix" {
		t.Fatalf("filesystems = %v", source.filesystems)
	}
	if len(vault.writes) != len(cfg.Credentials) {
		t.Fatalf("Vault writes = %d, want %d", len(vault.writes), len(cfg.Credentials))
	}
}
