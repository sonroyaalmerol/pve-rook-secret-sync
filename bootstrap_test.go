package main

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

type fakeBootstrapSource struct {
	keys          map[string]string
	rgw           map[string]map[string]string
	createdKeys   []string
	createdCaps   map[string][]string
	ensuredRGW    []string
	rgwWasCreated bool
}

func (source *fakeBootstrapSource) Key(_ context.Context, entity string) (string, error) {
	if key := source.keys[entity]; key != "" {
		return key, nil
	}
	return "", errors.New("missing")
}

func (source *fakeBootstrapSource) CreateKey(_ context.Context, entity string, caps []string, _ string) error {
	source.createdKeys = append(source.createdKeys, entity)
	source.createdCaps[entity] = caps
	return nil
}

func (source *fakeBootstrapSource) RGWCredentials(_ context.Context, uid string) (map[string]string, error) {
	if value := source.rgw[uid]; value != nil {
		return value, nil
	}
	return nil, errors.New("missing")
}

func (source *fakeBootstrapSource) EnsureRGWAdmin(_ context.Context, uid string) (bool, error) {
	source.ensuredRGW = append(source.ensuredRGW, uid)
	return source.rgwWasCreated, nil
}

func TestBootstrapCreatesOnlyMissingStandardUsers(t *testing.T) {
	cfg := config{
		Ceph: cephConfig{RGWPoolPrefix: "zone-a"},
		Credentials: []credentialSpec{
			{Entity: "client.healthchecker", Kind: "rook-mon"},
			{Entity: "client.csi-rbd-node", Kind: "rook-csi"},
			{Kind: "rook-config"},
			{Entity: "rgw-admin-ops-user", Kind: "rook-rgw-admin"},
		},
	}
	source := &fakeBootstrapSource{
		keys:          map[string]string{"client.csi-rbd-node": "key"},
		rgw:           map[string]map[string]string{},
		createdCaps:   map[string][]string{},
		rgwWasCreated: true,
	}
	var output bytes.Buffer
	if err := bootstrap(context.Background(), cfg, source, false, &output); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(source.createdKeys, []string{"client.healthchecker"}) {
		t.Fatalf("created CephX entities = %v", source.createdKeys)
	}
	if len(source.ensuredRGW) != 1 || source.ensuredRGW[0] != "rgw-admin-ops-user" {
		t.Fatalf("ensured RGW users = %v", source.ensuredRGW)
	}
	if !strings.Contains(strings.Join(source.createdCaps["client.healthchecker"], " "), "zone-a.rgw.meta") {
		t.Fatalf("healthchecker caps = %v", source.createdCaps["client.healthchecker"])
	}
	for _, line := range []string{"client.healthchecker: created", "client.csi-rbd-node: exists", "rgw-admin-ops-user: created"} {
		if !strings.Contains(output.String(), line) {
			t.Fatalf("output %q does not contain %q", output.String(), line)
		}
	}
}

func TestBootstrapDryRunAndCustomEntity(t *testing.T) {
	source := &fakeBootstrapSource{keys: map[string]string{}, rgw: map[string]map[string]string{}, createdCaps: map[string][]string{}}
	cfg := config{Credentials: []credentialSpec{{Entity: "client.csi-rbd-provisioner", Kind: "rook-csi"}, {Entity: "rgw-admin-ops-user", Kind: "rook-rgw-admin"}}}
	var output bytes.Buffer
	if err := bootstrap(context.Background(), cfg, source, true, &output); err != nil {
		t.Fatal(err)
	}
	if len(source.createdKeys) != 0 || len(source.ensuredRGW) != 0 || strings.Count(output.String(), "would-create") != 2 {
		t.Fatalf("unexpected dry run: output=%q source=%+v", output.String(), source)
	}

	cfg.Credentials = []credentialSpec{{Entity: "client.custom", Kind: "rook-csi"}}
	if err := bootstrap(context.Background(), cfg, source, false, &output); err == nil || !strings.Contains(err.Error(), "create it manually") {
		t.Fatalf("custom entity error = %v", err)
	}
}
