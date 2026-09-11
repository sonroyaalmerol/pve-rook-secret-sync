package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

const rookBundleVersion = "v1.20.5"

var rookBundleFiles = map[string]string{
	"crds.yaml":         "7fa7462d44b35d636703a03a88120030692a06a05ac91ec253200cb4ae8c37cf",
	"common.yaml":       "2d59b3093446dcf7dd69c73ebaef2d41e8cdfd14af564c2e885d49ab8d190457",
	"csi-operator.yaml": "de0bf9160c8370220353d7140a4b60ee6d4b8eda5e260f395fd00cbdc1059ffb",
	"operator.yaml":     "d5cd03549242a63a24dfc7a4d75a7f0c97efe954401a39666da58ff30f12dbeb",
}

type bundleOptions struct {
	SecretStoreName string
	SecretStoreKind string
	RBDPool         string
	CephFSName      string
	CephFSPool      string
}

func runBundle(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("bundle", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "", "path to JSON configuration")
	output := flags.String("output", "", "new directory for the GitOps bundle")
	secretStoreName := flags.String("secret-store", "", "External Secrets SecretStore name")
	secretStoreKind := flags.String("secret-store-kind", "ClusterSecretStore", "SecretStore or ClusterSecretStore")
	rbdPool := flags.String("rbd-pool", "", "existing Ceph RBD pool")
	cephFSName := flags.String("cephfs-name", "", "existing CephFS filesystem")
	cephFSPool := flags.String("cephfs-pool", "", "existing CephFS data pool")
	timeout := flags.Duration("timeout", 2*time.Minute, "overall operation timeout")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "unexpected argument %q\n", flags.Arg(0))
		return 2
	}
	if *output == "" || *secretStoreName == "" {
		fmt.Fprintln(stderr, "-output and -secret-store are required")
		return 2
	}
	if *secretStoreKind != "SecretStore" && *secretStoreKind != "ClusterSecretStore" {
		fmt.Fprintln(stderr, "-secret-store-kind must be SecretStore or ClusterSecretStore")
		return 2
	}
	if *rbdPool == "" && *cephFSName == "" {
		fmt.Fprintln(stderr, "-rbd-pool or -cephfs-name is required")
		return 2
	}
	if (*cephFSName == "") != (*cephFSPool == "") {
		fmt.Fprintln(stderr, "-cephfs-name and -cephfs-pool must be provided together")
		return 2
	}
	if *timeout <= 0 {
		fmt.Fprintln(stderr, "-timeout must be positive")
		return 2
	}
	path := *configPath
	if path == "" {
		path = preferredConfigPath(sharedConfigPath, localConfigPath)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if cfg.RookClusterName != "rook-ceph" {
		fmt.Fprintln(stderr, "bundle currently requires rook_cluster_name to be rook-ceph")
		return 1
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	external, err := renderExternalBundle(ctx, cfg, newCephSource(cfg.Ceph), bundleOptions{
		SecretStoreName: *secretStoreName,
		SecretStoreKind: *secretStoreKind,
		RBDPool:         *rbdPool,
		CephFSName:      *cephFSName,
		CephFSPool:      *cephFSPool,
	})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	upstream, err := downloadRookBundle(ctx, http.DefaultClient)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	upstream["external.yaml"] = []byte(external)
	upstream["kustomization.yaml"] = []byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - crds.yaml\n  - common.yaml\n  - csi-operator.yaml\n  - operator.yaml\n  - external.yaml\n")
	if err := writeBundle(*output, upstream); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "wrote Rook %s GitOps bundle to %s\n", rookBundleVersion, *output)
	return 0
}

func renderExternalBundle(ctx context.Context, cfg config, source cephReader, options bundleOptions) (string, error) {
	eligible, activeManager, err := source.CanSynchronize(ctx)
	if err != nil {
		return "", err
	}
	if !eligible {
		return "", fmt.Errorf("bundle must be rendered on active ceph manager %s", activeManager)
	}
	if _, err := source.FSID(ctx); err != nil {
		return "", err
	}
	monitors, err := source.Credential(ctx, credentialSpec{Kind: "rook-config"})
	if err != nil {
		return "", err
	}
	monData, err := externalMonData(monitors)
	if err != nil {
		return "", err
	}
	credentials := make(map[string]credentialSpec, len(cfg.Credentials))
	for _, credential := range cfg.Credentials {
		credential = activeCredential(credential, cfg.CephXGeneration)
		credentials[credential.Entity] = credential
	}
	mon, ok := credentials[activeCredential(credentialSpec{Entity: "client.healthchecker", Kind: "rook-mon"}, cfg.CephXGeneration).Entity]
	if !ok {
		return "", errors.New("client.healthchecker credential is required")
	}
	if options.RBDPool != "" {
		for _, entity := range []string{"client.csi-rbd-node", "client.csi-rbd-provisioner"} {
			if _, ok := credentials[activeCredential(credentialSpec{Entity: entity, Kind: "rook-csi"}, cfg.CephXGeneration).Entity]; !ok {
				return "", fmt.Errorf("%s credential is required for RBD", entity)
			}
		}
	}
	if options.CephFSName != "" {
		for _, entity := range []string{"client.csi-cephfs-node", "client.csi-cephfs-provisioner"} {
			if _, ok := credentials[activeCredential(credentialSpec{Entity: entity, Kind: "rook-cephfs-csi"}, cfg.CephXGeneration).Entity]; !ok {
				return "", fmt.Errorf("%s credential is required for CephFS", entity)
			}
		}
	}

	var manifest strings.Builder
	fmt.Fprintf(&manifest, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: rook-ceph-mon-endpoints\n  namespace: rook-ceph\ndata:\n  data: %s\n  mapping: %s\n  maxMonId: %s\n---\n", yamlQuote(monData), yamlQuote("{}"), yamlQuote("0"))
	manifest.WriteString("apiVersion: ceph.rook.io/v1\nkind: CephCluster\nmetadata:\n  name: rook-ceph\n  namespace: rook-ceph\nspec:\n  external:\n    enable: true\n  crashCollector:\n    disable: true\n---\n")
	writeExternalSecret(&manifest, cfg, options, "rook-ceph-mon", mon.VaultPath, map[string]string{
		"admin-secret": "admin-secret",
		"fsid":         "fsid",
		"mon-secret":   "mon-secret",
	})
	writeExternalSecret(&manifest, cfg, options, "rook-ceph-operator-creds", mon.VaultPath, map[string]string{
		"userID":  "ceph-username",
		"userKey": "ceph-secret",
	})
	for _, entity := range []string{"client.csi-rbd-node", "client.csi-rbd-provisioner", "client.csi-cephfs-node", "client.csi-cephfs-provisioner"} {
		credential, ok := credentials[activeCredential(credentialSpec{Entity: entity, Kind: credentialKind(entity)}, cfg.CephXGeneration).Entity]
		if !ok {
			continue
		}
		writeExternalSecret(&manifest, cfg, options, "rook-"+strings.TrimPrefix(entity, "client."), credential.VaultPath, map[string]string{"userID": "userID", "userKey": "userKey"})
	}
	if options.RBDPool != "" {
		fmt.Fprintf(&manifest, "apiVersion: storage.k8s.io/v1\nkind: StorageClass\nmetadata:\n  name: ceph-rbd\nprovisioner: rook-ceph.rbd.csi.ceph.com\nparameters:\n  clusterID: rook-ceph\n  pool: %s\n  imageFormat: %s\n  imageFeatures: layering\n  csi.storage.k8s.io/provisioner-secret-name: rook-csi-rbd-provisioner\n  csi.storage.k8s.io/provisioner-secret-namespace: rook-ceph\n  csi.storage.k8s.io/controller-expand-secret-name: rook-csi-rbd-provisioner\n  csi.storage.k8s.io/controller-expand-secret-namespace: rook-ceph\n  csi.storage.k8s.io/controller-publish-secret-name: rook-csi-rbd-provisioner\n  csi.storage.k8s.io/controller-publish-secret-namespace: rook-ceph\n  csi.storage.k8s.io/node-stage-secret-name: rook-csi-rbd-node\n  csi.storage.k8s.io/node-stage-secret-namespace: rook-ceph\n  csi.storage.k8s.io/fstype: ext4\nreclaimPolicy: Delete\nallowVolumeExpansion: true\n---\n", yamlQuote(options.RBDPool), yamlQuote("2"))
	}
	if options.CephFSName != "" {
		fmt.Fprintf(&manifest, "apiVersion: storage.k8s.io/v1\nkind: StorageClass\nmetadata:\n  name: cephfs\nprovisioner: rook-ceph.cephfs.csi.ceph.com\nparameters:\n  clusterID: rook-ceph\n  fsName: %s\n  pool: %s\n  csi.storage.k8s.io/provisioner-secret-name: rook-csi-cephfs-provisioner\n  csi.storage.k8s.io/provisioner-secret-namespace: rook-ceph\n  csi.storage.k8s.io/controller-expand-secret-name: rook-csi-cephfs-provisioner\n  csi.storage.k8s.io/controller-expand-secret-namespace: rook-ceph\n  csi.storage.k8s.io/controller-publish-secret-name: rook-csi-cephfs-provisioner\n  csi.storage.k8s.io/controller-publish-secret-namespace: rook-ceph\n  csi.storage.k8s.io/node-stage-secret-name: rook-csi-cephfs-node\n  csi.storage.k8s.io/node-stage-secret-namespace: rook-ceph\nreclaimPolicy: Delete\nallowVolumeExpansion: true\n---\n", yamlQuote(options.CephFSName), yamlQuote(options.CephFSPool))
	}
	return manifest.String(), nil
}

func writeExternalSecret(output *strings.Builder, cfg config, options bundleOptions, name, vaultPath string, properties map[string]string) {
	fmt.Fprintf(output, "apiVersion: external-secrets.io/v1\nkind: ExternalSecret\nmetadata:\n  name: %s\n  namespace: rook-ceph\nspec:\n  refreshInterval: 1m\n  secretStoreRef:\n    name: %s\n    kind: %s\n  target:\n    name: %s\n    creationPolicy: Owner\n    template:\n      type: kubernetes.io/rook\n  data:\n", yamlQuote(name), yamlQuote(options.SecretStoreName), yamlQuote(options.SecretStoreKind), yamlQuote(name))
	keys := make([]string, 0, len(properties))
	for key := range properties {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, secretKey := range keys {
		fmt.Fprintf(output, "    - secretKey: %s\n      remoteRef:\n        key: %s\n        property: %s\n", yamlQuote(secretKey), yamlQuote(strings.Trim(cfg.Vault.PathPrefix+"/"+vaultPath, "/")), yamlQuote(properties[secretKey]))
	}
	output.WriteString("---\n")
}

func credentialKind(entity string) string {
	if strings.Contains(entity, "cephfs") {
		return "rook-cephfs-csi"
	}
	return "rook-csi"
}

func externalMonData(values map[string]string) (string, error) {
	members := strings.Split(values["mon_initial_members"], ",")
	hosts := strings.Split(values["mon_host"], "],[")
	if len(members) == 0 || strings.TrimSpace(members[0]) == "" || len(hosts) == 0 || strings.TrimSpace(hosts[0]) == "" {
		return "", errors.New("render monitor endpoints: missing monitor data")
	}
	host := strings.Trim(hosts[0], "[] ")
	addresses := strings.Split(host, ",")
	address := strings.TrimSpace(addresses[len(addresses)-1])
	address = strings.TrimPrefix(strings.TrimPrefix(address, "v1:"), "v2:")
	if address == "" {
		return "", errors.New("render monitor endpoints: invalid monitor address")
	}
	return strings.TrimSpace(members[0]) + "=" + address, nil
}

func yamlQuote(value string) string {
	return strconv.Quote(value)
}

func downloadRookBundle(ctx context.Context, client *http.Client) (map[string][]byte, error) {
	result := make(map[string][]byte, len(rookBundleFiles))
	for name, checksum := range rookBundleFiles {
		url := "https://raw.githubusercontent.com/rook/rook/" + rookBundleVersion + "/deploy/examples/" + name
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, fmt.Errorf("create Rook manifest request: %w", err)
		}
		response, err := client.Do(request)
		if err != nil {
			return nil, fmt.Errorf("download %s: %w", name, err)
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 5<<20))
		closeErr := response.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("download %s: %w", name, readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("download %s: %w", name, closeErr)
		}
		if response.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("download %s: HTTP %s", name, response.Status)
		}
		actual := fmt.Sprintf("%x", sha256.Sum256(body))
		if actual != checksum {
			return nil, fmt.Errorf("download %s: checksum mismatch", name)
		}
		if name == "operator.yaml" {
			body, err = pinCephCSI(body)
			if err != nil {
				return nil, err
			}
		}
		result[name] = body
	}
	return result, nil
}

func pinCephCSI(manifest []byte) ([]byte, error) {
	oldImage := []byte("quay.io/cephcsi/cephcsi:v3.17.0")
	if bytes.Count(manifest, oldImage) != 1 {
		return nil, errors.New("pin Ceph-CSI image: expected source image not found once")
	}
	return bytes.Replace(manifest, oldImage, []byte("quay.io/cephcsi/cephcsi:v3.17.1"), 1), nil
}

func writeBundle(directory string, files map[string][]byte) error {
	if err := os.Mkdir(directory, 0o700); err != nil {
		return fmt.Errorf("create bundle directory: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(directory)
		}
	}()
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(directory, name), data, 0o600); err != nil {
			return fmt.Errorf("write bundle file %s: %w", name, err)
		}
	}
	ok = true
	return nil
}
