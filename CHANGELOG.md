# Changelog

## [2.0.0-rc](https://github.com/magenxcommerce/magenx-deputy-go/compare/v1.0.0...v2.0.0-rc) (2026-10-12)


### ⚠ BREAKING CHANGES

* artifact_digest is now required in the webhook payload. Set allow_missing_artifact_digest: true to accept payloads without it.

### 🚀 Features

* Verify artifact sha256 before extracting release ([#9](https://github.com/magenxcommerce/magenx-deputy-go/issues/9)) ([5a82771](https://github.com/magenxcommerce/magenx-deputy-go/commit/5a82771801b27d136d9ef9bc88de7ec54461d028))


### 🐛 Bug Fixes

* Harden deploy failure handling and rollback ([#10](https://github.com/magenxcommerce/magenx-deputy-go/issues/10)) ([e73bdc7](https://github.com/magenxcommerce/magenx-deputy-go/commit/e73bdc7dad0f073abf8f3db4611b8d95d36561dc))

## 1.0.0 (2026-10-12)


### 🐛 Bug Fixes

* Add magenx-deputy-go: S3-based Magento release deployment webhook ([#1](https://github.com/magenxcommerce/magenx-deputy-go/issues/1)) ([5cc5a78](https://github.com/magenxcommerce/magenx-deputy-go/commit/5cc5a7800e7bb8b19f6cdbefa975db0fb79c9711))
