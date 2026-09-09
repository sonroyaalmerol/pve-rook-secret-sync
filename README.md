# ceph-vault-sync

`ceph-vault-sync` copies existing CephX credentials into OpenBao or HashiCorp Vault KV v2 for delivery to Rook by External Secrets.

It does not create, rotate, or delete Ceph users. PVE or another Ceph administrator remains responsible for the CephX lifecycle.

## Features

- PVE-first SSH access with no PVE API dependency
- Local execution for cephadm, Rook, packages, and other Ceph distributions
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

## Configure

Copy `config.example.json` outside the repository and restrict its permissions:

```bash
cp config.example.json config.json
chmod 0600 config.json
```

The configuration does not contain Ceph keys or a Vault token. Authentication uses SSH configuration and the environment variable named by `vault.token_env`.

### PVE over SSH

```json
{
  "ceph": {
    "transport": "ssh",
    "host": "pve-node.example.com",
    "user": "root",
    "port": 22,
    "command": ["ceph"]
  }
}
```

Host-key checking follows the local OpenSSH configuration. Use `~/.ssh/config` for jump hosts, identity files, and aliases.

### Other Ceph distributions

Run on a host with an authenticated Ceph CLI:

```json
{
  "ceph": {
    "transport": "local",
    "command": ["ceph"]
  }
}
```

A command prefix can be included when required:

```json
{
  "ceph": {
    "transport": "local",
    "command": ["sudo", "-n", "ceph"]
  }
}
```

Any deployment that supports `ceph fsid` and `ceph auth get-key ENTITY` works.

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

Use a systemd timer, cron, or a trusted CI runner for periodic execution. Run it immediately after an administrator changes a CephX key.

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
