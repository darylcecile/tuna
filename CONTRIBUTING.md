# Contributing to tuna

Development requires Go 1.25 or later and Make on macOS or Linux.

## Build and run

```sh
git clone https://github.com/darylcecile/tuna.git
cd tuna
make build
./bin/tuna --help
```

`make build` produces the self-contained `bin/tuna` executable with CGO disabled.

To install your build into `~/.local/bin`:

```sh
make install
```

Put `~/.local/bin` on your PATH before running `tuna`. You can override the install prefix with `make install PREFIX=/your/prefix`.

To replace an existing installation with a local build:

```sh
tuna update --from ./bin/tuna
```

## Checks

```sh
make check
```

This runs race-enabled tests and `go vet`. By default, tests use temporary homes and fake analysis outputs, so they don't modify your harness settings or spend model credits. The service communicates over a user-private Unix socket; no listening TCP port or external database is needed.

To evaluate rule quality with an installed, signed-in harness:

```sh
TUNA_LIVE_ANALYZER=copilot go test ./internal/tuna -run '^TestLiveRuleQuality$' -count=1 -v
TUNA_LIVE_ANALYZER=copilot go test ./internal/tuna -run '^TestLiveFeedbackHistory$' -count=1 -v
```

Choose `copilot`, `opencode`, `claude`, or `codex`; optionally set `TUNA_LIVE_MODEL` to a model supported by that harness. Each command makes two real provider calls using your existing authentication and consumes model quota/credits. Tuna's database and published instructions stay in temporary homes. The evaluations cover rule quality, cross-session reinforcement, changes of preference, and instruction edits, and log the results for inspection. They are skipped unless `TUNA_LIVE_ANALYZER` is set.

## Release builds

```sh
make release VERSION=v0.1.0
```

This builds macOS and Linux binaries for ARM64 and AMD64 in `dist/`, alongside `checksums.txt`. `VERSION` sets the version reported by the binaries.

Pushing a `v*` tag triggers `.github/workflows/release.yaml`. It runs the checks, builds the binaries using the tag as their version, and publishes a GitHub Release with the binaries and checksums.
