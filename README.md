# Magento release deployer (Go)

A small webhook listener + deployer for Magento release archives stored in S3-compatible storage,
and for docker stack release bundles in the same storage (see [Docker stack deploys](#docker-stack-deploys)).

## Build

```bash
make build   # or: go build ./...
make test
```

## Install as systemd service

```bash
sudo make install
```

Installs:

- `/usr/local/sbin/magenx-deputy-go`
- `/etc/magenx-deputy-go/config.yaml` (from `config.example.yaml`, only if missing)
- `/lib/systemd/system/magenx-deputy-go.service`

The service runs as root and drops to `deploy.run_as` (the Magento site owner) for everything that touches
release files: tar, shared links, symlinks and commands. Releases and `bin/magento` output get the site
owner, and deputy refuses to start when deploy commands would run as root (`deploy.allow_root_commands`).
Root is needed to drive docker for [stack deploys](#docker-stack-deploys). Without stack deploys you can
also run it as the site user: `User=`/`Group=` the site owner and no `run_as`.

Set `deploy.run_as` in the config, adjust `ReadWritePaths=` in the unit (`systemctl edit magenx-deputy-go`)
to `<root_dir>/<brand>` and `stack.dir`, then:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now magenx-deputy-go
journalctl -u magenx-deputy-go -f
```

Optional `/etc/magenx-deputy-go/env` (mode 0600) is loaded as `EnvironmentFile`, e.g. `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`,
`REGISTRY_PASSWORD`, `IS_LEADER=false` on non-leader nodes. None of it reaches the commands (see `command_env`).

### Users and privileges

| Step | Runs as |
|---|---|
| webhook, HMAC, S3 downloads, digest checks, symlink switch, prune | deputy (root) |
| tar extraction, `pre_switch_commands`, `leader_commands`, `rollback_commands` | `deploy.run_as` |
| `docker`, `cosign`, stack tar extraction and stack commands | `stack.run_as` (empty = deputy) |

- Files and links deputy creates as root are handed to the `run_as` user.
- Archives are checked before extraction: absolute paths, `..`, symlinks or hard links leaving the release,
  devices, fifos and setuid/setgid/sticky bits are refused. tar runs with `--no-same-owner --no-same-permissions`.
- Shared links are only created inside the release: a release symlink that leads elsewhere is refused.
- Commands start with a clean environment: `HOME`, `USER`, `LOGNAME`, `PATH`, `RELEASE_DIR` and `command_env`.
  They run in the release dir.
- The unit limits root to `CAP_CHOWN CAP_DAC_OVERRIDE CAP_FOWNER CAP_SETUID CAP_SETGID CAP_KILL`, with
  `ProtectSystem=strict` and `ReadWritePaths=`. Access to the docker socket is still equal to root on the host.

Upgrading from 1.x: commands no longer inherit the deputy environment and run in the release dir; list the
variables they need in `deploy.command_env`. With the new root unit, set `deploy.run_as` to the user from the
old `User=`.

## Run

```bash
cp config.example.yaml config.yaml
./magenx-deputy-go -config config.yaml
```

Print build version:

```bash
./magenx-deputy-go version
```

## Webhook contract

The service expects:

- `X-Webhook-Secret: <secret>`
- `X-Signature: sha256=<hex-hmac-of-raw-json-body>`
- `X-Timestamp: <unix-seconds>`

JSON payload (code deploy, `"kind": "code"` or no `kind`):

```json
{
  "artifact_name": "release_2.4.7.tar.gz",
  "artifact_digest": "sha256:...",
  "timestamp": 1711111111,
  "brand": "optional-override",
  "domain": "optional-override"
}
```

`brand` and `domain` can be omitted when `deploy.default_brand` / `deploy.default_domain` are set.

`artifact_digest` is the sha256 of the release archive in S3, formatted `sha256:<64 hex chars>`.
It is required unless `allow_missing_artifact_digest: true` is set; a digest that is sent is always verified.

## Deployment behavior

1. validates webhook secret, HMAC signature, timestamp window and `artifact_digest`
2. checks free disk space
3. downloads the artifact from S3 into a staging dir `<releases_dir>/.<version>.tmp-*`, verifies sha256, extracts
4. renames staging to `<root_dir>/<brand>/<releases_dir_name>/<release_version>`; if that dir exists
   (redeploy of the same version) a new `<release_version>-<UTC timestamp>` dir is used, so the live
   release is never modified
5. mounts configured shared links
6. leader: runs `pre_switch_commands` in the new release while the old one keeps serving
7. switches `<root_dir>/<brand>/<current_link_suffix>` symlink atomically
8. leader: runs `leader_commands`
9. runs the health check (`attempts`, `retry_interval_seconds`)
10. prunes releases beyond `keep_releases` (never the new or previous one) and leftover staging dirs

Commands run with `/bin/sh -c` as `deploy.run_as` in the release dir; `{release_dir}` is replaced by the
shell-quoted release path and exported as `RELEASE_DIR`. The deploy runs to completion (or `deployment_timeout_seconds`)
even if the webhook client disconnects. The webhook answers after the deploy finished: `200 ok` or `500`
with the error.

## Failure handling

| Failure in | Result |
|---|---|
| disk check, download, digest, extract, shared links | staging/new dir removed, `current` untouched |
| `pre_switch_commands` | new dir removed, `current` untouched |
| `leader_commands`, health check | `current` switched back to the previous release (removed on first deploy), `rollback_commands` run in the previous release; rollback errors are added to the response |

Deputy does not touch the database on rollback; take a database backup before major upgrades.
Run `setup:upgrade` in `pre_switch_commands` (with `deployment/blue_green/enabled`) so a failed upgrade
never switches traffic.

Each node deploys independently; only the leader runs commands. The health check goes to `default_domain`,
which behind a load balancer may answer from another node.

All steps, failures and rollbacks are logged (`journalctl -u magenx-deputy-go`).

## Docker stack deploys

With `stack.enabled`, deputy also deploys releases of a docker configuration repository such as
[magenxcommerce-docker](https://github.com/magenxcommerce/magenxcommerce-docker), the same way as Magento
releases: the release workflow uploads a bundle to S3 and sends the webhook. It replaces the manual
`gh release download`, `verify_release.sh`, `pull`, `up -d`.

The bundle `stack_<tag>.tar.gz` holds the repository at the release tag plus `docker-compose.release.yml`,
which pins every image by digest. Next to it, `stack_<tag>.tar.gz.sig` is its signature (`cosign sign-blob
--bundle`). Both are in `<bucket>/<stack.s3_prefix>/`; bucket, endpoint, region and credentials are the ones of
the `deploy` section.

Payload, with the same webhook headers:

```json
{"kind": "stack", "tag": "v1.11.0", "artifact_digest": "sha256:...", "timestamp": 1711111111, "confirm": ["mariadb"]}
```

`artifact_name` defaults to `stack_<tag>.tar.gz`. `artifact_digest` is required unless
`allow_missing_artifact_digest`. Code and stack deploys share one lock: they never run at the same time.

Setup on the server:

1. Copy `cosign/cosign.pub` of the docker repository to `stack.cosign_public_key`. Deputy never trusts a key
   from the bundle: who can push to the repository could replace it.
2. Registry login for the image pulls: log in once as the `stack.run_as` user, or set `stack.registry_username`
   and put the password in `/etc/magenx-deputy-go/env` (`REGISTRY_PASSWORD`, for ghcr.io a classic PAT with
   `read:packages` only).
3. List server-only files in `stack.shared_links`, e.g. `/opt/<brand>/docker/.env.secrets` where `docker.sh`
   created it; each release links to it. `stack.env_files` are passed to compose as `--env-file`.
4. Add `stack.dir` to `ReadWritePaths=` and enable `stack` in the config.

Deployment behavior:

1. skips when `current` already is the tag (re-sent webhook)
2. downloads the bundle from S3 next to `<dir>/releases/.<tag>.tmp-*`, verifies its sha256 against
   `artifact_digest` and its signature with `cosign verify-blob` (skipped with `allow_unsigned_bundle`)
3. checks and extracts it into the staging dir
4. requires every image in `docker-compose.release.yml` pinned by digest and verifies each one with
   `cosign verify` (`verify-attestation --type spdxjson` with `require_sbom`)
5. moves it to `<dir>/releases/<tag>`, links `shared_links`, checks `env_files`
6. refuses image changes of `confirm_services` that the payload does not list in `confirm`
7. `docker compose config`, picks the services (`services`, or all minus `exclude_services` and minus services
   of profiles not in `profiles`), `docker login` (optional), `pull`
8. runs `pre_up_commands` in the new release; `current` still points to the old one
9. switches `<dir>/current`, runs `up --detach --no-build --wait --wait-timeout <wait_timeout_seconds>`
10. checks `health_check_url`, runs `post_up_commands`, prunes releases beyond `keep_releases`

Compose runs with `--project-name <project_name> --project-directory <dir>/releases/<tag>`, so unchanged images
keep their containers and volumes stay. Failure before the switch removes the new release dir and leaves the
running stack alone. Failure after the switch points `current` back, runs `up` of the previous release and
`rollback_commands`. The first stack deploy has no previous release: `current` is removed and the containers
are left as they are.

Rolling back images does not roll back data: a MariaDB or OpenSearch major upgrade rewrites its volume.
List such services in `confirm_services` and take a backup (e.g. in `pre_up_commands`) before you confirm.
