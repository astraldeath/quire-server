# CI caches

The [Server checks workflow](../.github/workflows/checks.yml) uses separate caches for Go tests, container smoke tests and image publication.

## Cache keys and scopes

The test job caches the Go module and compilation directories reported by `go env GOMODCACHE` and `go env GOCACHE` inside the build container. The key includes runner OS/architecture, Go 1.27.1, `go.sum`, and a commit suffix. Restore fallback stays within the same dependency lock and toolchain. Update the key's Go version with the container version; bump `go-v1` to invalidate all entries.

The container smoke test uses Buildx with a loaded local image and the GitHub Actions layer cache under `server-check`. Publishing uses the separate `server-publish` scope so the single-platform smoke image cannot evict the multi-platform publication cache.

## Cache writes and invalidation

Go and smoke-image cache saves are restricted to non-PR main/version-tag runs in the canonical repository. Publication uses the same gate. Pull requests restore eligible caches but do not export these caches. No runtime data, credentials, or generated backup files are included; the smoke test's data and backups stay inside its disposable container. Docker caches contain build layers, including public reader sources and compiled binaries, and are not secret storage.

Builds also work without cached data. Clear repository Actions caches or bump the Go key/Docker scope if invalidation is needed. Cache hits and Docker/native builds must be checked on a GitHub runner; actionlint validates the workflows locally.
