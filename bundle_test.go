package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderExternalBundle(t *testing.T) {
	cfg := testConfig()
	cfg.Vault.PathPrefix = "rook/production"
	cfg.Credentials = append(cfg.Credentials,
		credentialSpec{VaultPath: "rook-csi-rbd-provisioner", Entity: "client.csi-rbd-provisioner", Kind: "rook-csi"},
		credentialSpec{VaultPath: "rook-csi-cephfs-node", Entity: "client.csi-cephfs-node", Kind: "rook-cephfs-csi"},
		credentialSpec{VaultPath: "rook-csi-cephfs-provisioner", Entity: "client.csi-cephfs-provisioner", Kind: "rook-cephfs-csi"},
	)
	source := fakeCeph{
		fsid: "cluster-a",
		values: map[string]map[string]string{
			"": {
				"mon_host":            "[v2:10.0.0.1:3300,v1:10.0.0.1:6789],[v2:10.0.0.2:3300,v1:10.0.0.2:6789]",
				"mon_initial_members": "a,b",
			},
		},
	}
	manifest, err := renderExternalBundle(context.Background(), cfg, source, bundleOptions{
		SecretStoreName: "vault",
		SecretStoreKind: "ClusterSecretStore",
		RBDPool:         "kubernetes",
		CephFSName:      "phoenix",
		CephFSPool:      "phoenix_data",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"data: \"a=10.0.0.1:6789\"",
		"kind: CephCluster",
		"name: \"rook-ceph-operator-creds\"",
		"key: \"rook/production/rook-ceph-mon\"",
		"name: ceph-rbd",
		"pool: \"kubernetes\"",
		"name: cephfs",
		"fsName: \"phoenix\"",
	} {
		if !strings.Contains(manifest, want) {
			t.Errorf("manifest does not contain %q", want)
		}
	}
}

func TestRenderExternalBundleRequiresCredentials(t *testing.T) {
	cfg := testConfig()
	source := fakeCeph{
		fsid: "cluster-a",
		values: map[string]map[string]string{
			"": {"mon_host": "[v1:10.0.0.1:6789]", "mon_initial_members": "a"},
		},
	}
	_, err := renderExternalBundle(context.Background(), cfg, source, bundleOptions{RBDPool: "pool"})
	if err == nil || !strings.Contains(err.Error(), "provisioner") {
		t.Fatalf("error = %v", err)
	}
}

func TestPinCephCSI(t *testing.T) {
	manifest, err := pinCephCSI([]byte(`plugin: "quay.io/cephcsi/cephcsi:v3.17.0"`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(manifest), "cephcsi:v3.17.1") {
		t.Fatalf("manifest = %q", manifest)
	}
	if _, err := pinCephCSI(manifest); err == nil {
		t.Fatal("unexpected source image was accepted")
	}
}

func TestWriteBundleRefusesExistingDirectory(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "bundle")
	if err := writeBundle(directory, map[string][]byte{"file.yaml": []byte("data")}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(directory, "file.yaml")); err != nil || string(got) != "data" {
		t.Fatalf("bundle file = %q, %v", got, err)
	}
	if err := writeBundle(directory, map[string][]byte{"file.yaml": []byte("changed")}); err == nil {
		t.Fatal("existing bundle directory was overwritten")
	}
}
