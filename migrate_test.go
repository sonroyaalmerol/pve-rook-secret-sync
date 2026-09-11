package main

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"strings"
	"testing"
)

type fakeMigrationSource struct {
	fakeRotationSource
	states      map[string]authKeyState
	helperApply []bool
	staged      []string
	installed   []string
	restarted   []string
	stuck       bool
}

func (source *fakeMigrationSource) RunMigrationHelper(_ context.Context, apply bool) ([]byte, error) {
	source.helperApply = append(source.helperApply, apply)
	return []byte("client.admin: migrated\n"), nil
}

func (source *fakeMigrationSource) AuthKeyStates(context.Context) (map[string]authKeyState, error) {
	states := make(map[string]authKeyState, len(source.states))
	maps.Copy(states, source.states)
	return states, nil
}

func (source *fakeMigrationSource) StagePendingKey(_ context.Context, entity string) (string, error) {
	source.staged = append(source.staged, entity)
	state := source.states[entity]
	state.Pending = secureKeyType
	source.states[entity] = state
	return "pending-" + entity, nil
}

func (source *fakeMigrationSource) InstallRGWKey(_ context.Context, daemon rgwDaemonConfig, key string) error {
	if key != "pending-"+daemon.Entity {
		return errUnexpectedKey
	}
	source.installed = append(source.installed, daemon.Entity)
	return nil
}

func (source *fakeMigrationSource) RestartRGW(_ context.Context, daemon rgwDaemonConfig) error {
	source.restarted = append(source.restarted, daemon.Entity)
	if source.stuck {
		return nil
	}
	state := source.states[daemon.Entity]
	state.Current, state.Pending = state.Pending, ""
	source.states[daemon.Entity] = state
	return nil
}

var errUnexpectedKey = errors.New("unexpected keyring content")

func observedMigrationSource(stuck bool) *fakeMigrationSource {
	return &fakeMigrationSource{
		fakeRotationSource: fakeRotationSource{
			fakeCeph: fakeCeph{fsid: "cluster-a", keys: map[string]string{
				"client.healthchecker": "old-mon",
				"client.csi-rbd-node":  "old-rbd",
			}},
			version: cephVersion{Major: 19, Minor: 2, Patch: 6},
		},
		states: map[string]authKeyState{
			"client.admin":                        {Current: secureKeyType},
			"client.healthchecker":                {Current: "aes"},
			"client.csi-rbd-node":                 {Current: "aes"},
			"client.rgw.k8s-staging.10.254.23.51": {Current: "aes"},
			"client.rgw.k8s-staging.10.254.23.52": {Current: "aes"},
			"client.rgw.k8s-staging.10.254.23.53": {Current: "aes"},
		},
		stuck: stuck,
	}
}

func observedRGWDaemons() []rgwDaemonConfig {
	daemons := make([]rgwDaemonConfig, 0, 3)
	for _, address := range []string{"10.254.23.51", "10.254.23.52", "10.254.23.53"} {
		daemons = append(daemons, rgwDaemonConfig{
			Entity:  "client.rgw.k8s-staging." + address,
			Host:    address,
			Unit:    "ceph-radosgw@k8s-staging." + address + ".service",
			Keyring: "/var/lib/ceph/radosgw/ceph-rgw.k8s-staging." + address + "/keyring",
		})
	}
	return daemons
}

func TestMigrateToSecureKeysCoversHelperRGWAndRook(t *testing.T) {
	cfg := testConfig()
	cfg.Ceph.RGWDaemons = observedRGWDaemons()
	source := observedMigrationSource(false)
	vault := &fakeVault{values: map[string]vaultValue{}, writes: map[string]map[string]string{}}
	var output bytes.Buffer

	next, err := migrateToSecureKeys(context.Background(), cfg, source, vault, false, &output)
	if err != nil {
		t.Fatal(err)
	}
	if len(source.helperApply) != 1 || !source.helperApply[0] {
		t.Fatalf("migration helper calls = %v", source.helperApply)
	}
	want := "client.rgw.k8s-staging.10.254.23.51,client.rgw.k8s-staging.10.254.23.52,client.rgw.k8s-staging.10.254.23.53"
	for name, got := range map[string][]string{"staged": source.staged, "installed": source.installed, "restarted": source.restarted} {
		if strings.Join(got, ",") != want {
			t.Errorf("%s = %q", name, strings.Join(got, ","))
		}
	}
	if next.CephXGeneration != 1 {
		t.Fatalf("generation = %d, want 1", next.CephXGeneration)
	}
	if vault.writes["rook-csi-rbd-node"]["userID"] != "csi-rbd-node.1" {
		t.Fatalf("Rook secrets were not published: %+v", vault.writes["rook-csi-rbd-node"])
	}
	if !strings.Contains(output.String(), "client.admin: migrated") {
		t.Fatalf("helper output was not forwarded: %q", output.String())
	}
}

func TestMigrateToSecureKeysKeepsOldKeyWhenDaemonDoesNotAuthenticate(t *testing.T) {
	cfg := testConfig()
	cfg.Ceph.RGWDaemons = observedRGWDaemons()[:1]
	source := observedMigrationSource(true)
	vault := &fakeVault{values: map[string]vaultValue{}, writes: map[string]map[string]string{}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := migrateToSecureKeys(ctx, cfg, source, vault, false, &bytes.Buffer{})
	if err == nil {
		t.Fatal("unpromoted key was accepted")
	}
	if len(vault.writes) != 0 || len(source.created) != 0 {
		t.Fatalf("failed RGW migration still changed Rook credentials: %+v", vault.writes)
	}
}

func TestMigrateToSecureKeysDryRunChangesNothing(t *testing.T) {
	cfg := testConfig()
	cfg.Ceph.RGWDaemons = observedRGWDaemons()[:1]
	source := observedMigrationSource(false)
	vault := &fakeVault{values: map[string]vaultValue{}, writes: map[string]map[string]string{}}
	var output bytes.Buffer

	if _, err := migrateToSecureKeys(context.Background(), cfg, source, vault, true, &output); err != nil {
		t.Fatal(err)
	}
	if source.helperApply[0] || len(source.staged) != 0 || len(source.installed) != 0 || len(source.restarted) != 0 || len(source.created) != 0 || len(vault.writes) != 0 {
		t.Fatalf("dry run changed state: %+v", source)
	}
	if !strings.Contains(output.String(), "would stage "+secureKeyType) || !strings.Contains(output.String(), "would-create with "+secureKeyType) {
		t.Fatalf("unexpected plan %q", output.String())
	}
}

func TestMigrateToSecureKeysSkipsRotatedRookGeneration(t *testing.T) {
	cfg := testConfig()
	cfg.CephXGeneration = 2
	source := observedMigrationSource(false)
	source.states["client.healthchecker.2"] = authKeyState{Current: secureKeyType}
	source.states["client.csi-rbd-node.2"] = authKeyState{Current: secureKeyType}
	vault := &fakeVault{values: map[string]vaultValue{}, writes: map[string]map[string]string{}}
	var output bytes.Buffer

	next, err := migrateToSecureKeys(context.Background(), cfg, source, vault, false, &output)
	if err != nil {
		t.Fatal(err)
	}
	if next.CephXGeneration != 2 || len(source.created) != 0 {
		t.Fatalf("generation = %d, created = %v", next.CephXGeneration, source.created)
	}
	if !strings.Contains(output.String(), "already uses "+secureKeyType) {
		t.Fatalf("unexpected output %q", output.String())
	}
}

func TestParseAuthKeyStates(t *testing.T) {
	states, err := parseAuthKeyStates([]byte(`{"data":{"secrets":[
		{"entity":{"type_str":"client","id":"admin"},"auth":{"key":{"type_str":"aes"},"pending_key":{"type_str":"aes256k"}}},
		{"entity":{"type_str":"client","id":"rgw.k8s-staging.10.254.23.51"},"auth":{"key":{"type_str":"aes256k"},"pending_key":{"type_str":"none"}}}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if states["client.admin"] != (authKeyState{Current: "aes", Pending: secureKeyType}) {
		t.Fatalf("admin state = %+v", states["client.admin"])
	}
	if states["client.rgw.k8s-staging.10.254.23.51"] != (authKeyState{Current: secureKeyType}) {
		t.Fatalf("RGW state = %+v", states["client.rgw.k8s-staging.10.254.23.51"])
	}
	if _, err := parseAuthKeyStates([]byte(`{"data":{"secrets":[]}}`)); err == nil {
		t.Fatal("empty inventory was accepted")
	}
}
