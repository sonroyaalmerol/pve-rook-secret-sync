# pve-rook-secret-sync

`pve-rook-secret-sync` manages Ceph credentials and cluster metadata in OpenBao or HashiCorp Vault KV v2 for Rook consumers.

The resident `sync` command is read-only toward Ceph. The explicit `bootstrap`, `migrate`, and `rotate` commands create, migrate, and rotate secrets. Rotation uses a new identity generation and keeps every previous identity valid.

## Features

- PVE-first local execution with no PVE API dependency
- Idempotent one-shot bootstrap for standard Rook CephX and RGW admin users
- Staged AES256K CephX rotation with old and new generations preserved in Vault
- One-pass AES256K migration for admin, cluster-owned, RGW daemon, and Rook keys
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
go build -o pve-rook-secret-sync .
```

Tagged releases publish Linux amd64 and arm64 archives, Debian packages, and SHA-256 checksums through GitHub Actions:

```bash
git tag v0.1.0
git push origin v0.1.0
```

Install a release package on every PVE node:

```bash
apt install ./pve-rook-secret-sync_0.1.0_linux_amd64.deb
```

The package does not enable the service before it is configured.

## Commands

```bash
pve-rook-secret-sync COMMAND [FLAGS]
```

| Command     | Purpose                                                         | Ceph access |
| ----------- | --------------------------------------------------------------- | ----------- |
| `init`      | write a new configuration file                                  | none        |
| `bootstrap` | create missing standard CephX users and the RGW admin ops user  | read-write  |
| `sync`      | copy current credentials and cluster metadata to Vault          | read-only   |
| `migrate`   | move admin, cluster-owned, RGW daemon, and Rook keys to AES256K | read-write  |
| `rotate`    | create the next CephX identity generation and publish it        | read-write  |

Every command accepts `-h`. Exit status is `0` on success, `1` on failure, and `2` for a usage error or, with `sync -check`, for detected drift.

`bootstrap`, `sync`, `migrate`, and `rotate` share three flags:

| Flag                | Default                                                                                        | Meaning                           |
| ------------------- | ---------------------------------------------------------------------------------------------- | --------------------------------- |
| `-config FILE`      | `/etc/pve/priv/pve-rook-secret-sync/config.json`, then `/etc/pve-rook-secret-sync/config.json` | explicit configuration file       |
| `-dry-run`          | off                                                                                            | print the plan and change nothing |
| `-timeout DURATION` | `30s` for `bootstrap` and `sync`, `2m` for `rotate`, `1h` for `migrate`                        | deadline for the whole run        |

`sync` adds two more:

| Flag                 | Default | Meaning                                                           |
| -------------------- | ------- | ----------------------------------------------------------------- |
| `-check`             | off     | exit `2` when Vault differs, without writing; excludes `-dry-run` |
| `-interval DURATION` | `0`     | keep running and repeat at this interval instead of exiting       |

`init` writes the configuration and never contacts Ceph or Vault:

| Flag                        | Default                                              | Meaning                                                     |
| --------------------------- | ---------------------------------------------------- | ----------------------------------------------------------- |
| `-vault-address URL`        | `VAULT_ADDR`                                         | Vault or OpenBao server URL, required                       |
| `-path-prefix PATH`         |                                                      | Vault path prefix for this cluster, required                |
| `-output FILE`              | `/etc/pve/priv/pve-rook-secret-sync/config.json`     | file to create, never overwritten                           |
| `-vault-mount NAME`         | `service-secrets`                                    | KV v2 mount                                                 |
| `-vault-namespace NAME`     | `VAULT_NAMESPACE`                                    | Vault Enterprise namespace                                  |
| `-ca-cert FILE`             | `VAULT_CACERT`                                       | CA bundle for the Vault server certificate                  |
| `-token-file FILE`          | `/etc/pve/priv/pve-rook-secret-sync/vault-token`     | token file referenced by the configuration                  |
| `-auth-method NAME`         | `token`                                              | `token`, `userpass`, or `approle`                           |
| `-auth-mount PATH`          | the method name                                      | auth mount path                                             |
| `-auth-username NAME`       |                                                      | userpass username                                           |
| `-auth-password-file FILE`  | `/etc/pve/priv/pve-rook-secret-sync/vault-password`  | userpass password file                                      |
| `-auth-role-id ID`          |                                                      | AppRole role ID                                             |
| `-auth-secret-id-file FILE` | `/etc/pve/priv/pve-rook-secret-sync/vault-secret-id` | AppRole secret ID file                                      |
| `-rook-cluster-name NAME`   | `rook-ceph`                                          | Rook cluster name recorded in the configuration             |
| `-dashboard`                | off                                                  | add the dashboard link credential                           |
| `-rgw`                      | off                                                  | add the `rgw-admin-ops-user` credential                     |
| `-rgw-realm NAME`           |                                                      | multisite realm for `radosgw-admin`                         |
| `-rgw-zonegroup NAME`       |                                                      | multisite zonegroup for `radosgw-admin`                     |
| `-rgw-zone NAME`            |                                                      | multisite zone for `radosgw-admin`                          |
| `-rgw-pool-prefix NAME`     | `default`                                            | pool prefix used in the `client.healthchecker` capabilities |

## Configuration reference

`ceph` selects how Ceph commands run:

| Key                                      | Default                                                               | Meaning                                                  |
| ---------------------------------------- | --------------------------------------------------------------------- | -------------------------------------------------------- |
| `transport`                              | `local`, or `ssh` when `host` is set                                  | run commands locally or over SSH                         |
| `host`                                   |                                                                       | remote host for `ssh` transport                          |
| `user`                                   | `root`                                                                | SSH user, also used for RGW daemon hosts                 |
| `port`                                   | `22`                                                                  | SSH port                                                 |
| `command`                                | `["ceph"]`                                                            | Ceph CLI and any prefix such as `sudo -n`                |
| `rgw_command`                            | `["radosgw-admin"]`                                                   | RGW admin CLI and any prefix                             |
| `migration_helper`                       | `["/usr/share/pve-manager/migrations/pve-cephx-rotate-service-keys"]` | Proxmox CephX migration helper used by `migrate`         |
| `rgw_realm`, `rgw_zonegroup`, `rgw_zone` |                                                                       | multisite selectors passed to `radosgw-admin`            |
| `rgw_pool_prefix`                        | `default`                                                             | pool prefix in the `client.healthchecker` capabilities   |
| `rgw_daemons`                            | discovered from `ceph service dump`                                   | override list of `entity`, `host`, `unit`, and `keyring` |
| `coordination`                           | `active-manager`                                                      | `active-manager` or `none`                               |
| `manager_name`                           | local hostname                                                        | manager identity when it differs from the hostname       |

`vault` selects the server and how to authenticate with it:

| Key           | Default           | Meaning                                                                     |
| ------------- | ----------------- | --------------------------------------------------------------------------- |
| `address`     | `VAULT_ADDR`      | server URL, HTTPS unless `allow_http` is set                                |
| `namespace`   | `VAULT_NAMESPACE` | Vault Enterprise namespace                                                  |
| `mount`       |                   | KV v2 mount, required                                                       |
| `path_prefix` |                   | prefix below the mount, required and unique per Ceph cluster                |
| `token_env`   | `VAULT_TOKEN`     | environment variable holding the token                                      |
| `token_file`  |                   | file holding the token, preferred over `token_env`                          |
| `ca_cert`     | `VAULT_CACERT`    | CA bundle for the server certificate                                        |
| `allow_http`  | `false`           | permit a plain HTTP address away from localhost                             |
| `auth`        | token lookup      | `method`, `mount`, `username`, `password_file`, `role_id`, `secret_id_file` |

The remaining keys are at the top level:

| Key                 | Default     | Meaning                                                          |
| ------------------- | ----------- | ---------------------------------------------------------------- |
| `rook_cluster_name` | `rook-ceph` | Rook cluster name                                                |
| `cephx_generation`  | `0`         | active identity generation, maintained by `rotate` and `migrate` |
| `credentials`       |             | one entry per Vault path                                         |

Each credential has a `vault_path`, a `kind`, and, for user credentials, an `entity`. `user_id` overrides the published `userID` when it differs from the entity name. The `kind` selects what is read from Ceph:

| Kind              | Source                    | Published fields                                                                     |
| ----------------- | ------------------------- | ------------------------------------------------------------------------------------ |
| `rook-mon`        | `ceph auth get-key`       | `cluster-name`, `fsid`, `admin-secret`, `mon-secret`, `ceph-username`, `ceph-secret` |
| `rook-csi`        | `ceph auth get-key`       | `userID`, `userKey`                                                                  |
| `rook-cephfs-csi` | `ceph auth get-key`       | `userID`, `userKey`, `adminID`, `adminKey`                                           |
| `rook-config`     | `ceph mon dump`           | `mon_host`, `mon_initial_members`                                                    |
| `rook-dashboard`  | `ceph mgr services`       | `userID` fixed to `ceph-dashboard-link`, `userKey` holding the URL                   |
| `rook-rgw-admin`  | `radosgw-admin user info` | `accessKey`, `secretKey`                                                             |

The packaged `/etc/pve-rook-secret-sync/config.json` remains a local fallback and a template for non-PVE systems. `-config FILE` always selects an explicit file.

Authentication uses `vault.token_file` when set, otherwise the environment variable named by `vault.token_env`. A token file is reread for every synchronization, so rotating it does not require restarting the service.

### Auth methods

The default token method reads `vault.token_file` or `vault.token_env`. Username/password and AppRole logins are also supported through a `vault.auth` block; when set, they replace token lookup entirely:

```json
{
  "vault": {
    "auth": {
      "method": "userpass",
      "mount": "userpass",
      "username": "ceph-sync",
      "password_file": "/etc/pve/priv/pve-rook-secret-sync/vault-password"
    }
  }
}
```

AppRole uses a role ID plus a secret ID file:

```json
{
  "vault": {
    "auth": {
      "method": "approle",
      "role_id": "00000000-0000-0000-0000-000000000000",
      "secret_id_file": "/etc/pve/priv/pve-rook-secret-sync/vault-secret-id"
    }
  }
}
```

`mount` defaults to the method name. Login tokens are cached in memory and reused until shortly before their lease expires, then re-obtained automatically; a revoked cached token triggers one immediate re-login and retry. Passwords and secret IDs are read from files for every login, so rotating either takes effect without a restart. LDAP, OIDC, and other interactive auth methods are not supported.

`init` can generate these blocks directly:

```bash
pve-rook-secret-sync init -vault-address https://vault.example.com:8200 -path-prefix rook-pve/staging \
  -auth-method userpass -auth-username ceph-sync
pve-rook-secret-sync init -vault-address https://vault.example.com:8200 -path-prefix rook-pve/staging \
  -auth-method approle -auth-role-id 00000000-0000-0000-0000-000000000000
```

### PVE cluster

Generate the shared configuration once, on any cluster node:

```bash
pve-rook-secret-sync init \
  -vault-address https://vault.example.com:8200 \
  -vault-namespace example \
  -path-prefix rook-pve/staging \
  -dashboard \
  -rgw
```

`init` writes `/etc/pve/priv/pve-rook-secret-sync/config.json` with local Ceph access, active-manager coordination, the five standard CephX credentials, monitor configuration, and optional dashboard and RGW credentials. It points at `/etc/pve/priv/pve-rook-secret-sync/vault-token` without creating it. It creates parent directories and refuses to overwrite an existing configuration. Use `-output`, `-token-file`, `-vault-mount`, `-ca-cert`, or `-rook-cluster-name` to change those defaults. Multisite RGW deployments can also set `-rgw-realm`, `-rgw-zonegroup`, `-rgw-zone`, and `-rgw-pool-prefix`.

Preview and create missing users before starting the service:

```bash
pve-rook-secret-sync bootstrap -dry-run
pve-rook-secret-sync bootstrap
```

Bootstrap recognizes only the five standard entity names generated by `init`. Existing CephX users and their caps are left unchanged. When RGW is enabled, bootstrap creates or reuses `rgw-admin-ops-user` and ensures its `info=read` capability. The two per-bucket COSI users remain owned by Rook and are not copied to Vault.

Rotate all standard CephX identities to the next AES256K generation after upgrading the provider to Ceph 19.2.6 or newer:

```bash
pve-rook-secret-sync rotate -dry-run
pve-rook-secret-sync rotate
```

A rotation from generation 0 creates identities such as `client.csi-rbd-node.1`. Before changing `cephx_generation`, the command writes both generations to `<vault_path>/generations/0` and `<vault_path>/generations/1`, publishes generation 1 at the stable path, and atomically updates the configuration. Repeating the command creates the next generation. It never deletes an old identity.

Use Rook v1.19.9 or v1.20.5 or newer and Ceph-CSI v3.17.1 or newer before publishing AES256K keys. Kubernetes storage nodes need Linux 7.0 or newer, or Linux 7.2 or newer in FIPS mode. Drain each storage node after External Secrets publishes the new `userID` and `userKey`; existing mounts can continue with the retained old identity during the rollout.

`migrate` clears the `AUTH_INSECURE_CLIENT_KEY_TYPE` warning for every key this tool can reach, in one pass:

```bash
pve-rook-secret-sync migrate -dry-run
pve-rook-secret-sync migrate
```

It runs the Proxmox helper for `client.admin` and the cluster-owned monitor, manager, OSD, and MDS keys, migrates every RGW daemon, then rotates the Rook credentials to a new AES256K generation and publishes them to Vault. Rook credentials are skipped when the active generation already uses AES256K. Only Rook consumer secrets reach Vault; `client.admin` and RGW daemon keys stay on the provider.

RGW daemon keys are not owned by the Proxmox helper. They are discovered from `ceph service dump`, which reports the id and host of every registered gateway. Each daemon's `ceph-radosgw@<id>.service` unit and keyring are then resolved on that host, preferring the keyring the daemon's own configuration names. List `ceph.rgw_daemons` only to override that discovery, for example when a gateway uses a custom unit or a host name that does not resolve:

```json
{
  "ceph": {
    "migration_helper": [
      "/usr/share/pve-manager/migrations/pve-cephx-rotate-service-keys"
    ],
    "rgw_daemons": [
      {
        "entity": "client.rgw.k8s-staging.10.254.23.51",
        "host": "vm-lan-1",
        "unit": "ceph-radosgw@k8s-staging.10.254.23.51.service",
        "keyring": "/var/lib/ceph/radosgw/ceph-rgw.k8s-staging.10.254.23.51/keyring"
      }
    ]
  }
}
```

A daemon on the node running the command is handled locally. Reaching any other daemon needs root SSH access from this node to its host. On a PVE cluster that works without setup: host keys are pinned to `/etc/pve/nodes/<node>/ssh_known_hosts` under the node name, as `PVE::SSHInfo` does, because plain SSH between PVE nodes fails host key verification.

Listing any daemon replaces discovery entirely. Daemons migrate one at a time. Each one gets a pending AES256K key next to its current key, receives the new keyring over SSH, and restarts. Ceph promotes a pending key only when the daemon authenticates with it, so the promotion is the proof that the restart succeeded. A daemon that does not come back keeps its working key and stops the run before any later daemon or Rook credential changes.

Restrict the monitors to AES256K with `ceph mon set auth_allowed_ciphers aes256k` only after `ceph health detail` reports no remaining insecure key, including keys held by consumers this tool does not manage. Restricting ciphers while any key is incompatible can make the cluster unavailable.

Write the token separately so it never appears in command arguments:

```bash
read -rsp 'Vault token: ' VAULT_TOKEN
printf '%s\n' "$VAULT_TOKEN" > /etc/pve/priv/pve-rook-secret-sync/vault-token
unset VAULT_TOKEN
```

The service automatically prefers the generated shared configuration and falls back to `/etc/pve-rook-secret-sync/config.json` when the shared file does not exist.

`pmxcfs` replicates the private directory across the cluster and makes it root-only. It controls permissions by path, so do not run `chmod` inside `/etc/pve`. Store only small, infrequently changed configuration there, not logs or runtime state.

Each run asks the Ceph monitors for the active manager. Only that manager's host reads credentials or accesses Vault; standby hosts exit successfully. Manager names are matched against the local short or fully qualified hostname. Clusters whose manager daemon IDs differ from their hostnames must retain node-local configurations and set `ceph.manager_name` separately.

Ceph monitor consensus provides one active manager during normal operation. Vault compare-and-set remains the final write guard. Existing `_ceph_fsid` metadata also prevents a different Ceph cluster from taking over the same Vault paths. Keep `vault.path_prefix` unique per Ceph cluster.

### Other Ceph distributions

Any deployment that supports `ceph mgr dump --format json`, `ceph fsid`, and `ceph auth get-key ENTITY` can use active-manager coordination. Monitor metadata additionally uses `ceph mon dump`; dashboard discovery uses `ceph mgr services`; RGW credentials use `radosgw-admin`.

The mutating commands need more. `bootstrap` and `rotate` use `ceph auth get-or-create`, with `--key-type` for AES256K. `migrate` also uses `ceph version`, `ceph auth dump-keys`, `ceph auth get-or-create-pending`, and `ceph service dump`, runs the configured `migration_helper`, and reaches each RGW host over SSH for `systemctl` and `ceph-conf`. Only `migrate` and its helper are specific to Proxmox VE.

Command prefixes can be included when required:

```json
{
  "ceph": {
    "transport": "local",
    "command": ["sudo", "-n", "ceph"],
    "rgw_command": ["sudo", "-n", "radosgw-admin"]
  }
}
```

For one external runner, set `coordination` to `none`. Do not use `none` on every node in a cluster. SSH transport remains available; `ceph.host` identifies the remote manager unless `ceph.manager_name` is set explicitly.

### systemd service on every PVE node

The Debian package installs the binary, a local fallback configuration, an optional Vault environment file, and the service and timer units. It does not write package-managed files into `/etc/pve`.

Enable the service on every node after creating the shared files:

```bash
systemctl enable --now pve-rook-secret-sync.service
```

The service waits for `pve-cluster.service`, synchronizes immediately, then polls every 15 seconds. Shared configuration, token-file, and CA certificate changes are picked up by every node during polling. Active-manager coordination ensures only one node reads credentials or accesses Vault. The compatibility timer remains packaged for installations upgrading from the older timer-based service.

Reloading triggers an immediate synchronization on that node. `SIGUSR1` does the same. No restart is needed after changing a token file; environment-variable changes still require a restart.

```bash
systemctl reload pve-rook-secret-sync.service
systemctl kill --kill-whom=main --signal=SIGUSR1 pve-rook-secret-sync.service
systemctl restart pve-rook-secret-sync.service
```

## Synchronize

```bash
export VAULT_TOKEN='...'
pve-rook-secret-sync sync -config config.json
```

With `-dashboard -rgw`, the generated configuration writes these KV v2 paths below `service-secrets/rook-pve/staging`:

- `rook-ceph-mon`
- `rook-ceph-config`
- `rook-csi-rbd-node`
- `rook-csi-rbd-provisioner`
- `rook-csi-cephfs-node`
- `rook-csi-cephfs-provisioner`
- `rook-ceph-dashboard-link`
- `rgw-admin-ops-user`

The payloads match Rook's field names: `mon_host` and `mon_initial_members` for config, `userID` and `userKey` for the dashboard link, and `accessKey` and `secretKey` for RGW admin ops. Each entry also contains `_ceph_fsid`, `_sync_generation`, and `_synced_at`; user credentials include `_ceph_entity`. Map only the Rook fields into destination Kubernetes Secrets.

Preview changes:

```bash
pve-rook-secret-sync sync -config config.json -dry-run
```

Check for drift without writing. Exit status 2 means at least one path differs:

```bash
pve-rook-secret-sync sync -config config.json -check
```

The resident service detects CephX keys, monitor metadata, dashboard links, and RGW admin key changes within the polling interval. A rotation workflow can run `systemctl reload pve-rook-secret-sync.service` for immediate synchronization.

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

`rotate` never replaces a key in place. It creates a numeric identity generation, preserves both generations at distinct Vault paths, publishes the new generation at the stable paths, and updates `cephx_generation`. Previous CephX identities remain valid until an administrator verifies that every Kubernetes node has remounted with the new identity and removes them explicitly. This tool does not infer that a node drain or remount has completed.
