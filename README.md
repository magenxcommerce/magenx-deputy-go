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

## Deployment behavior

- validates webhook secret, HMAC signature, and timestamp window
- downloads artifact from S3 using `github.com/aws/aws-sdk-go-v2/service/s3`
- extracts release into `<root_dir>/<brand>/<releases_dir_name>/<release_version>`
- mounts configured shared links
- switches `<root_dir>/<brand>/<current_link_suffix>` symlink to the new release
- runs leader commands when `IS_LEADER` (or configured env var) resolves true
- runs configurable health check
- rolls back the symlink if post-switch steps fail
