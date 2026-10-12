# Magento release deployer (Go)

A small webhook listener + deployer for Magento release archives stored in S3-compatible storage.

## Build

```bash
go build ./...
```

## Run

```bash
cp config.example.yaml config.yaml
./magenx-deputy-go -config config.yaml
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
