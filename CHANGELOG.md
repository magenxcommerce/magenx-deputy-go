# Changelog

## [2.0.2-rc](https://github.com/magenxcommerce/magenx-deputy-go/compare/v2.0.1...v2.0.2-rc) (2026-11-09)


### 🐛 Bug Fixes

* Integer conversion of uids and gids ([#41](https://github.com/magenxcommerce/magenx-deputy-go/issues/41)) ([bd373f5](https://github.com/magenxcommerce/magenx-deputy-go/commit/bd373f56217107d5ebcf73f1fd5b77f9cc868775))

## [2.0.1](https://github.com/magenxcommerce/magenx-deputy-go/compare/v2.0.0...v2.0.1) (2026-11-09)


### 🐛 Bug Fixes

* Address CodeQL request-forgery, path-injection and log-injection alerts, create tags for draft releases ([#39](https://github.com/magenxcommerce/magenx-deputy-go/issues/39)) ([f3e486c](https://github.com/magenxcommerce/magenx-deputy-go/commit/f3e486cfad17708d9cee35178a779e0128f85ece))


### 🚦 Maintenance

* **main:** magenx-deputy-go 2.0.1-rc ([#32](https://github.com/magenxcommerce/magenx-deputy-go/issues/32)) ([ec76ccd](https://github.com/magenxcommerce/magenx-deputy-go/commit/ec76ccd0174d9478091fc9d7186fa4f2dccf7e78))

## [2.0.1-rc](https://github.com/magenxcommerce/magenx-deputy-go/compare/v2.0.0...v2.0.1-rc) (2026-11-09)


### 🐛 Bug Fixes

* Address CodeQL request-forgery, path-injection and log-injection alerts, create tags for draft releases ([#39](https://github.com/magenxcommerce/magenx-deputy-go/issues/39)) ([f3e486c](https://github.com/magenxcommerce/magenx-deputy-go/commit/f3e486cfad17708d9cee35178a779e0128f85ece))

## [2.0.0](https://github.com/magenxcommerce/magenx-deputy-go/compare/v1.0.0...v2.0.0) (2026-11-08)


### ⚠ BREAKING CHANGES

* stack.repo, api_url, token_env, allow_prerelease, require_immutable, poll_interval_seconds and lock_asset are gone; lock_asset is now lock_file. The webhook needs artifact_digest.
* commands no longer inherit the deputy environment and run in the release dir; pass variables with deploy.command_env. The unit now runs as root: set deploy.run_as to the user from the old User=.
* artifact_digest is now required in the webhook payload. Set allow_missing_artifact_digest: true to accept payloads without it.

### 🚀 Features

* Deploy docker stack releases from GitHub, run as root with run_as ([#28](https://github.com/magenxcommerce/magenx-deputy-go/issues/28)) ([44133ba](https://github.com/magenxcommerce/magenx-deputy-go/commit/44133ba478033e7d214cb01622e1b9030c19776a))
* Deploy stack bundles from S3 instead of GitHub ([#30](https://github.com/magenxcommerce/magenx-deputy-go/issues/30)) ([e00a274](https://github.com/magenxcommerce/magenx-deputy-go/commit/e00a27464aa8ba0f5a9a8d6a63e58529a7cd8b69))
* Verify artifact sha256 before extracting release ([#9](https://github.com/magenxcommerce/magenx-deputy-go/issues/9)) ([5a82771](https://github.com/magenxcommerce/magenx-deputy-go/commit/5a82771801b27d136d9ef9bc88de7ec54461d028))


### 🐛 Bug Fixes

* Harden deploy failure handling and rollback ([#10](https://github.com/magenxcommerce/magenx-deputy-go/issues/10)) ([e73bdc7](https://github.com/magenxcommerce/magenx-deputy-go/commit/e73bdc7dad0f073abf8f3db4611b8d95d36561dc))


### 📝 Documentation

* Clarify database handling on rollback ([#11](https://github.com/magenxcommerce/magenx-deputy-go/issues/11)) ([02c2f27](https://github.com/magenxcommerce/magenx-deputy-go/commit/02c2f276565a60f24ca4a1fba80b944a07e6344f))


### 🚦 Maintenance

* **main:** magenx-deputy-go 1.0.0 ([#12](https://github.com/magenxcommerce/magenx-deputy-go/issues/12)) ([8a58cd1](https://github.com/magenxcommerce/magenx-deputy-go/commit/8a58cd143d94517864b4f453a4462f773488a2c3))
* **main:** magenx-deputy-go 2.0.0-rc ([#29](https://github.com/magenxcommerce/magenx-deputy-go/issues/29)) ([8eaf551](https://github.com/magenxcommerce/magenx-deputy-go/commit/8eaf551dcfb022f358d0f5666688c91a1184fded))
* **main:** magenx-deputy-go 2.0.0-rc ([#8](https://github.com/magenxcommerce/magenx-deputy-go/issues/8)) ([a958107](https://github.com/magenxcommerce/magenx-deputy-go/commit/a9581075a42f02e6d3d05de8dcbe189a5ba206e5))

## [2.0.0-rc](https://github.com/magenxcommerce/magenx-deputy-go/compare/v1.0.0...v2.0.0-rc) (2026-11-08)


### ⚠ BREAKING CHANGES

* stack.repo, api_url, token_env, allow_prerelease, require_immutable, poll_interval_seconds and lock_asset are gone; lock_asset is now lock_file. The webhook needs artifact_digest.
* commands no longer inherit the deputy environment and run in the release dir; pass variables with deploy.command_env. The unit now runs as root: set deploy.run_as to the user from the old User=.
* artifact_digest is now required in the webhook payload. Set allow_missing_artifact_digest: true to accept payloads without it.

### 🚀 Features

* Deploy docker stack releases from GitHub, run as root with run_as ([#28](https://github.com/magenxcommerce/magenx-deputy-go/issues/28)) ([44133ba](https://github.com/magenxcommerce/magenx-deputy-go/commit/44133ba478033e7d214cb01622e1b9030c19776a))
* Deploy stack bundles from S3 instead of GitHub ([#30](https://github.com/magenxcommerce/magenx-deputy-go/issues/30)) ([e00a274](https://github.com/magenxcommerce/magenx-deputy-go/commit/e00a27464aa8ba0f5a9a8d6a63e58529a7cd8b69))
* Verify artifact sha256 before extracting release ([#9](https://github.com/magenxcommerce/magenx-deputy-go/issues/9)) ([5a82771](https://github.com/magenxcommerce/magenx-deputy-go/commit/5a82771801b27d136d9ef9bc88de7ec54461d028))


### 🐛 Bug Fixes

* Harden deploy failure handling and rollback ([#10](https://github.com/magenxcommerce/magenx-deputy-go/issues/10)) ([e73bdc7](https://github.com/magenxcommerce/magenx-deputy-go/commit/e73bdc7dad0f073abf8f3db4611b8d95d36561dc))


### 📝 Documentation

* Clarify database handling on rollback ([#11](https://github.com/magenxcommerce/magenx-deputy-go/issues/11)) ([02c2f27](https://github.com/magenxcommerce/magenx-deputy-go/commit/02c2f276565a60f24ca4a1fba80b944a07e6344f))


### 🚦 Maintenance

* **main:** magenx-deputy-go 1.0.0 ([#12](https://github.com/magenxcommerce/magenx-deputy-go/issues/12)) ([8a58cd1](https://github.com/magenxcommerce/magenx-deputy-go/commit/8a58cd143d94517864b4f453a4462f773488a2c3))
* **main:** magenx-deputy-go 2.0.0-rc ([#8](https://github.com/magenxcommerce/magenx-deputy-go/issues/8)) ([a958107](https://github.com/magenxcommerce/magenx-deputy-go/commit/a9581075a42f02e6d3d05de8dcbe189a5ba206e5))

## 1.0.0 (2026-10-12)


### ⚠ BREAKING CHANGES

* artifact_digest is now required in the webhook payload. Set allow_missing_artifact_digest: true to accept payloads without it.

### 🚀 Features

* Verify artifact sha256 before extracting release ([#9](https://github.com/magenxcommerce/magenx-deputy-go/issues/9)) ([5a82771](https://github.com/magenxcommerce/magenx-deputy-go/commit/5a82771801b27d136d9ef9bc88de7ec54461d028))


### 🐛 Bug Fixes

* Add magenx-deputy-go: S3-based Magento release deployment webhook ([#1](https://github.com/magenxcommerce/magenx-deputy-go/issues/1)) ([5cc5a78](https://github.com/magenxcommerce/magenx-deputy-go/commit/5cc5a7800e7bb8b19f6cdbefa975db0fb79c9711))
* Harden deploy failure handling and rollback ([#10](https://github.com/magenxcommerce/magenx-deputy-go/issues/10)) ([e73bdc7](https://github.com/magenxcommerce/magenx-deputy-go/commit/e73bdc7dad0f073abf8f3db4611b8d95d36561dc))


### 📝 Documentation

* Clarify database handling on rollback ([#11](https://github.com/magenxcommerce/magenx-deputy-go/issues/11)) ([02c2f27](https://github.com/magenxcommerce/magenx-deputy-go/commit/02c2f276565a60f24ca4a1fba80b944a07e6344f))


### 🚦 Maintenance

* **main:** magenx-deputy-go 1.0.0 ([#7](https://github.com/magenxcommerce/magenx-deputy-go/issues/7)) ([46bc5ea](https://github.com/magenxcommerce/magenx-deputy-go/commit/46bc5eaf276e6ab88b59f6062b2976b77d40f798))
* **main:** magenx-deputy-go 2.0.0-rc ([#8](https://github.com/magenxcommerce/magenx-deputy-go/issues/8)) ([a958107](https://github.com/magenxcommerce/magenx-deputy-go/commit/a9581075a42f02e6d3d05de8dcbe189a5ba206e5))

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
