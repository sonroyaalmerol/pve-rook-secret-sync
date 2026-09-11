# ceph-vault-sync

`ceph-vault-sync` copies existing CephX credentials into OpenBao or HashiCorp Vault KV v2 for delivery to Rook by External Secrets.

It does not create, rotate, or delete Ceph users. PVE or another Ceph administrator remains responsible for the CephX lifecycle.

## Features

- PVE-first local execution with no PVE API dependency
- Active Ceph manager coordination for conflict-free multi-host deployment
- Local or SSH execution for cephadm, Rook, packages, and other Ceph distributions
- OpenBao and Vault KV v2 HTTP API support
- Idempotent writes
- KV compare-and-set protection against concurrent updates
- Shared generation identifiers across every synchronized credential
- Dry-run and drift-check modes
- Secret values never printed
- Standard library only

## Build

Go 1.24 or newer is required.

```bash
go build -o ceph-vault-sync .
```

Tagged releases publish Linux amd64 and arm64 archives, Debian packages, and SHA-256 checksums through GitHub Actions:

```bash
git tag v0.1.0
git push origin v0.1.0
```

Install a release package on each PVE node, then edit the packaged example configuration and Vault environment file:

```bash
apt install ./ceph-vault-sync_0.1.0_linux_amd64.deb
editor /etc/ceph-vault-sync/config.json
editor /etc/default/ceph-vault-sync
systemctl enable --now ceph-vault-sync.service
```

The package does not enable the service before those files are configured.

## Configure

Copy `config.example.json` outside the repository and restrict its permissions:

```bash
cp config.example.json config.json
chmod 0600 config.json
```

The configuration does not contain Ceph keys or a Vault token. Authentication uses the environment variable named by `vault.token_env`.

### PVE cluster

Install the same binary and configuration on every PVE node. Local execution and active-manager coordination are the defaults:

```json
{
  "ceph": {
    "transport": "local",
    "command": ["ceph"],
    "coordination": "active-manager"
  }
}
```

Each run asks the Ceph monitors for the active manager. Only that manager's host reads credentials or accesses Vault; standby hosts exit successfully. Manager names are matched against the local short or fully qualified hostname. Set `ceph.manager_name` separately on each host only when its manager daemon ID does not match its hostname.

Ceph monitor consensus provides one active manager during normal operation. Vault compare-and-set remains the final write guard. Existing `_ceph_fsid` metadata also prevents a different Ceph cluster from taking over the same Vault paths. Keep `vault.path_prefix` unique per Ceph cluster.

### Other Ceph distributions

Any deployment that supports `ceph mgr dump --format json`, `ceph fsid`, and `ceph auth get-key ENTITY` can use active-manager coordination. A command prefix can be included when required:

```json
{
  "ceph": {
    "transport": "local",
    "command": ["sudo", "-n", "ceph"]
  }
}
```

For one external runner, set `coordination` to `none`. Do not use `none` on every node in a cluster. SSH transport remains available; `ceph.host` identifies the remote manager unless `ceph.manager_name` is set explicitly.

### systemd service on every PVE node

The Debian package installs the binary at `/usr/sbin/ceph-vault-sync`, the configuration at `/etc/ceph-vault-sync/config.json`, the root-only Vault environment file at `/etc/default/ceph-vault-sync`, and the service and timer units under `/lib/systemd/system`.

Set `VAULT_TOKEN` in `/etc/default/ceph-vault-sync`, then enable the service after configuring every node:

```bash
chmod 0600 /etc/ceph-vault-sync/config.json /etc/default/ceph-vault-sync
systemctl enable --now ceph-vault-sync.service
```

The service synchronizes immediately, then polls every 15 seconds. Active-manager coordination ensures only one cluster node reads credentials or accesses Vault. The timer remains packaged so installations upgrading from the older timer-based service still start the resident service after boot.

Reloading triggers an immediate synchronization and rereads the JSON configuration and CA certificate. `SIGUSR1` also triggers an immediate synchronization. Restart the service after changing `/etc/default/ceph-vault-sync` because a running process cannot inherit changed environment variables.

```bash
systemctl reload ceph-vault-sync.service
systemctl kill --kill-whom=main --signal=SIGUSR1 ceph-vault-sync.service
systemctl restart ceph-vault-sync.service
```

## Synchronize

```bash
export VAULT_TOKEN='...'
ceph-vault-sync sync -config config.json
```

The example writes these KV v2 paths below `service-secrets/rook-pve/staging`:

- `rook-ceph-mon`
- `rook-csi-rbd-node`
- `rook-csi-rbd-provisioner`
- `rook-csi-cephfs-node`
- `rook-csi-cephfs-provisioner`

Each entry contains the exact Rook fields plus `_ceph_entity`, `_ceph_fsid`, `_sync_generation`, and `_synced_at` metadata. Map only the Rook fields into destination Kubernetes Secrets.

Preview changes:

```bash
ceph-vault-sync sync -config config.json -dry-run
```

Check for drift without writing. Exit status 2 means at least one path differs:

```bash
ceph-vault-sync sync -config config.json -check
```

The resident service detects CephX key changes within the polling interval. A rotation workflow can run `systemctl reload ceph-vault-sync.service` for immediate synchronization.

## External Secrets

Use one `ExternalSecret` per destination Secret. Keep the Kubernetes Secret names stable while the `userID` and `userKey` values change.

```yaml
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: rook-csi-rbd-node
  namespace: rook-ceph
spec:
  refreshInterval: 15s
  secretStoreRef:
    kind: ClusterSecretStore
    name: vault-secrets-store
  target:
    name: rook-csi-rbd-node
    creationPolicy: Merge
    deletionPolicy: Retain
    template:
      type: kubernetes.io/rook
  data:
    - secretKey: userID
      remoteRef:
        key: rook-pve/staging/rook-csi-rbd-node
        property: userID
    - secretKey: userKey
      remoteRef:
        key: rook-pve/staging/rook-csi-rbd-node
        property: userKey
```

Use `creationPolicy: Merge` and `deletionPolicy: Retain` while adopting existing staging Secrets. For a fresh installation, use `creationPolicy: Owner`. Use explicit `data` mappings rather than `dataFrom` so synchronization metadata does not enter the Rook Secret.

## Vault policy

The runner needs read and update access to KV data and read access to KV metadata. Adjust the mount and prefix as needed:

```hcl
path "service-secrets/data/rook-pve/staging/*" {
  capabilities = ["create", "read", "update"]
}

path "service-secrets/metadata/rook-pve/staging/*" {
  capabilities = ["read"]
}
```

External Secrets should use a separate read-only policy.

## Rotation behavior

This tool intentionally does not rotate users. If a Ceph administrator replaces a key in place, the old key stops working before any synchronizer can copy the new key. For zero-downtime rotation, create a second CephX identity, synchronize it, verify the new generation, and only then remove the old identity.
