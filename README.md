# Magento release deployer (Go)

A small webhook listener + deployer for Magento release archives stored in S3-compatible storage or an OCI
registry such as ghcr.io, and for docker stack release bundles in the same storage (see
[Docker stack deploys](#docker-stack-deploys) and [OCI registry source](#oci-registry-source)).
`magenx-deputy-go bootstrap` deploys both on a new server without a webhook ([Bootstrap](#bootstrap)).

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
| webhook, HMAC, S3 and registry downloads, digest checks, `cosign` on code and OCI bundle manifests, symlink switch, prune | deputy (root) |
| tar extraction, release files | `deploy.run_as` |
| `pre_switch_commands`, `leader_commands`, `rollback_commands` | `deploy.commands_run_as` (empty = `deploy.run_as`) |
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

Commands run with `/bin/sh -c` as `deploy.commands_run_as` (default `deploy.run_as`) in the release dir; `{release_dir}` is replaced by the
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
running stack alone. Failure after the switch points `current` back, runs `restore_commands`, `up` of the
previous release and `rollback_commands`. The first stack deploy has no previous release: `current` is removed
and the containers are left as they are.

`restore_commands` run in the previous release whenever a deploy fails after `pre_up_commands` started: on a
rollback before its `up`, and after a failure before the switch. They undo what `pre_up_commands` changed
outside the release. Example: secrets encrypted with SOPS in the docker repository (`scripts/secrets.sh`),
decrypted to tmpfs, where `.env.secrets` of every release leads:

```yaml
stack:
  pre_up_commands: ["bash scripts/secrets.sh decrypt"]   # new release's .env.secrets.enc to tmpfs before up
  restore_commands: ["bash scripts/secrets.sh decrypt"]  # previous release's back on failure
  command_env: ["SOPS_AGE_KEY_FILE=/root/.config/sops/age/keys.txt", "STACK_ENV=production"]
```

`STACK_ENV` is the server's environment (`staging` on staging servers): it picks `.env.secrets.<env>.enc`.
Staging servers also load the committed staging override: `env_files: [.env, .env.staging, .env.secrets]`.

Add the tmpfs folder (`/run/<brand>-secrets`) to `ReadWritePaths=`: `ProtectSystem=strict` makes `/run`
read-only for deputy.

Rolling back images does not roll back data: a MariaDB or OpenSearch major upgrade rewrites its volume.
List such services in `confirm_services` and take a backup (e.g. in `pre_up_commands`) before you confirm.

### Docker hosts

On a docker host the release files belong to the container user (userns-remap: `run_as: "165532:165532"`),
who has no docker access, while `bin/magento` must run in a container. `commands_run_as: root` with
`allow_root_commands: true` runs the commands as root, the files stay owned by `run_as`:

```yaml
deploy:
  run_as: "165532:165532"
  commands_run_as: root
  allow_root_commands: true
  pre_switch_commands:
    - docker compose -p mystore -f /opt/mystore/stack/current/docker-compose.yml run --rm --no-deps -w {release_dir} magento setup:upgrade --keep-generated
```

Root commands run with the same rights as the stack commands (`stack.run_as` empty): the docker socket.

## OCI registry source

With `source: oci` (`deploy.source`, `stack.source`) a release is an image in a registry: one manifest whose
only layer is the archive itself, as `crane append` pushes it, so the layer digest is the sha256 of the archive.

```bash
crane append -f release_v1.4.0.tar.gz -t ghcr.io/acme/magento-release:v1.4.0
crane mutate --annotation org.opencontainers.image.version=v1.4.0 ghcr.io/acme/magento-release:v1.4.0
cosign sign --key cosign.key ghcr.io/acme/magento-release@<manifest digest>
crane tag ghcr.io/acme/magento-release@<manifest digest> production   # promotion: a tag, no copy
```

The webhook sends the tag and the layer digest: `{"tag": "v1.4.0", "artifact_digest": "sha256:..."}` for code
(the release dir is the tag), the same with `"kind": "stack"` for the stack. Deputy resolves `<repository>:<tag>`,
requires exactly one layer whose digest is `artifact_digest`, checks the manifest signature, then downloads the
layer and checks its sha256 again before extraction:

- code: the signature is checked when `deploy.cosign_public_key` is set (required by bootstrap).
- stack: always, with `stack.cosign_public_key`, unless `allow_unsigned_bundle`. It replaces the `.sig` file of S3.

`cosign` runs as deputy itself with `deploy.cosign_binary` / `stack.cosign_binary` and their `cosign_args`. With
`oci.username` set it gets the registry login through a temporary `DOCKER_CONFIG` (mode 0700, removed after the
check), never on its command line. Registry login of deputy's own pulls is `oci` in the config, the password
in the environment variable `oci.password_env` (default `REGISTRY_PASSWORD`, so one read-only PAT serves
deputy and `stack.registry_username`). A registry on a loopback or private address is reached over http.

## Bootstrap

`magenx-deputy-go bootstrap` deploys the release a moving tag points to, without a webhook: on a new server
from cloud-init, before the service starts.

```bash
# the variables of the service's EnvironmentFile: REGISTRY_PASSWORD, IS_LEADER, ...
set -a; . /etc/magenx-deputy-go/env; set +a
magenx-deputy-go bootstrap -config /etc/magenx-deputy-go/config.yaml -tag production
systemctl enable --now magenx-deputy-go
```

1. refuses to run while `listen_addr` is in use (the service would deploy into the same dirs)
2. stack (`stack.enabled`): resolves `<stack.oci_repository>:<tag>`, reads the release version from the manifest
   annotation `org.opencontainers.image.version`, then deploys it like a webhook with that version and the
   layer digest: `<repository>:<version>` must hold the same archive, the signature must verify
3. code: the same with `deploy.oci_repository`; skipped when `current` already is that version. Code runs
   after the stack, so `pre_switch_commands` (setup:upgrade) find the database running

Nothing in bootstrap is covered by the webhook HMAC, so the signatures are the trust anchor: it needs source
`oci` for both, `stack.allow_unsigned_bundle: false` and `deploy.cosign_public_key`.

Flags: `-tag` (default `production`), `-stack-tag`, `-code-tag`, `-skip-stack`, `-skip-code`, and `-confirm mariadb,...`
for image changes of `confirm_services` on a server that already runs a stack. Exit status 0 on success.
