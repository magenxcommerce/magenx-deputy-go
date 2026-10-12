# Magento release deployer (Go)

A small webhook listener + deployer for Magento release archives stored in S3-compatible storage.

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

The service runs as the Magento site owner, not root, so releases and `bin/magento` output get correct ownership.
Adjust `User=`, `Group=` and `ReadWritePaths=` in the unit (`systemctl edit magenx-deputy-go`) to the brand user and `<root_dir>/<brand>`, then:

```bash
sudo chgrp mystore /etc/magenx-deputy-go/config.yaml   # config is 0640, site user must read it
sudo systemctl daemon-reload
sudo systemctl enable --now magenx-deputy-go
journalctl -u magenx-deputy-go -f
```

Optional `/etc/magenx-deputy-go/env` (mode 0600) is loaded as `EnvironmentFile`, e.g. `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `IS_LEADER=false` on non-leader nodes.

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

JSON payload:

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

Commands run with `/bin/sh -c` as the service user; `{release_dir}` is replaced by the shell-quoted
release path and exported as `RELEASE_DIR`. The deploy runs to completion (or `deployment_timeout_seconds`)
even if the webhook client disconnects. The webhook answers after the deploy finished: `200 ok` or `500`
with the error.

## Failure handling

| Failure in | Result |
|---|---|
| disk check, download, digest, extract, shared links | staging/new dir removed, `current` untouched |
| `pre_switch_commands` | new dir removed, `current` untouched. Database changes already made by the command are **not** reverted |
| `leader_commands`, health check | `current` switched back to the previous release (removed on first deploy), `rollback_commands` run in the previous release; rollback errors are added to the response |

Database migrations are never rolled back. Keep schema changes backward compatible so the previous release
still runs on the upgraded database, and run `setup:upgrade` in `pre_switch_commands` with
`deployment/blue_green/enabled` so a failed upgrade never switches traffic.

Each node deploys independently; only the leader runs commands. The health check goes to `default_domain`,
which behind a load balancer may answer from another node.

All steps, failures and rollbacks are logged (`journalctl -u magenx-deputy-go`).
