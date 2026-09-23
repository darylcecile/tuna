# tuna

**An ear for feedback. A memory for better work.**

Tuna learns how you like coding agents to work. Start it once, work normally, and let useful corrections become shared preferences across OpenCode, Copilot CLI, Codex, and Claude Code.

## Install

Tuna runs on macOS and Linux, on ARM64 and AMD64. You'll need an installed, authenticated coding harness to provide the analysis model.

### With Binzo

Install [Binzo](https://github.com/darylcecile/binzo), then run:

```sh
binzo add darylcecile/tuna --name tuna
```

Binzo selects the release binary for your system and installs it as `tuna`. Follow its PATH instructions if this is your first Binzo install.

### From GitHub Releases

Download your binary and `checksums.txt` from the [latest release](https://github.com/darylcecile/tuna/releases/latest):

| System | Download |
| --- | --- |
| macOS, Apple Silicon | `tuna_darwin_arm64` |
| macOS, Intel | `tuna_darwin_amd64` |
| Linux, ARM64 | `tuna_linux_arm64` |
| Linux, AMD64 / x86-64 | `tuna_linux_amd64` |

Check the file's SHA-256 hash against its entry in `checksums.txt` using `shasum -a 256 <filename>` on macOS or `sha256sum <filename>` on Linux. Then install it on your PATH. For example, from the download directory on an Apple Silicon Mac:

```sh
mkdir -p "$HOME/.local/bin"
install -m 755 tuna_darwin_arm64 "$HOME/.local/bin/tuna"
```

Replace the filename for other systems. If `~/.local/bin` isn't on your PATH, add `export PATH="$HOME/.local/bin:$PATH"` to your shell configuration and open a new terminal.

## Get started

```sh
tuna start
```

`tuna start` installs integrations for harnesses found on PATH, links their global instructions, and launches a detached service. It runs until stopped or the machine shuts down; run `tuna start` again after reboot. New sessions pick up the hooks and instructions. OpenCode reloads its global plugin automatically. In Codex, open `/hooks` and trust the new tuna hook.

## Everyday commands

```sh
tuna help
tuna status
tuna list today
tuna list 'week ago'              # Last seven calendar days through now
tuna list 2026-09-23
tuna query 'tests'
tuna query --category scope --json
tuna list --model 'provider/model' --all
tuna remove 12 15                 # Forget notes and their published rules
tuna consolidate                  # Process the queue and refresh instructions now
tuna stop
tuna restart
tuna update                       # Update a direct-download installation
```

For Binzo-managed installs, update with `binzo update darylcecile/tuna`, then run `tuna restart` to refresh the service and integrations.

Output is plain, readable text by default. `--json` provides structured output for scripts and agents. Preference IDs stay stable. Reads work while the service is stopped; capture, removal, and consolidation require it to be running.

## Configure

```sh
tuna setup --analyzer copilot
tuna setup --analyzer opencode --model 'provider/model'
tuna setup --at 22:30
tuna setup --stale-days 90         # 0 disables age-based expiry
tuna setup codex                  # Explicitly install an integration
tuna setup agents                 # Only link instruction files
```

Settings live in `~/.local/share/tuna/config.json`. `--home` or `TUNA_HOME` changes the data directory. `--agents-file /absolute/path/AGENTS.md` selects a different shared instruction file. Restart after hand-editing config; `setup` restarts a running service automatically.

The default analyzer is the first installed CLI in this order: Copilot, OpenCode, Claude, Codex. Tuna uses that harness's existing authentication and default model. Select another explicitly if the first isn't signed in. Background analysis consumes that provider's normal usage allowance. Batches run every 30 seconds by default (`batch_seconds` in config).

## What it learns

Prompt hooks enqueue user messages and, when available, recent assistant context. The service asks the selected harness to distinguish actionable corrections from ordinary requests, software bug reports, quotations, and frustration without a reusable preference. Polite corrections qualify too. Each learned note records a category, exact supporting quote, harness, session, source model, and timestamp. Missing model metadata is labeled `unknown`, never inferred by the analyst.

Only textual feedback exposed by the hooks is captured; UI thumbs-up/down reactions that a harness does not expose are not collected. Tuna observes new prompts after setup, not historical sessions. The analyzer gets a dedicated working directory, a recursion guard, and restricted tools.

At 23:00 local time, while running, tuna processes pending feedback and consolidates active preferences into the shared `AGENTS.md`. If asleep at that time, it runs on the first tick later that same day. `tuna consolidate` runs it on demand. Semantic duplicates and genuine conflicts are reconciled in favor of the latest applicable correction; model-specific conditions are retained. Unrefreshed notes expire after 180 days by default.

Learned rules live in a marked section. The analyst can also propose exact-text edits to existing instructions, but only when backed by a retained preference and needed to reconcile duplicates or conflicts. Unrelated text is preserved. Each changed instruction file is backed up under tuna's `backups/`. Model judgments can be imperfect: inspect `tuna list`, remove unwanted preferences, or restore a backup as needed. `remove` clears the supporting quote and published rule, retaining the note ID and removed status for history; it does not erase previous file backups or reverse prior edits to hand-written instructions.

The local SQLite database stores pending messages until successful analysis, then clears their raw payloads. Retained notes keep the relevant supporting quotation. Messages are sent to your selected harness's model provider for analysis; “local database” does not mean offline inference. Failed analysis stays queued and is shown by `tuna status`, with diagnostics in `service.log`.

## Harness integrations

| Harness | Capture | Shared instructions | Preference tools |
| --- | --- | --- | --- |
| OpenCode V2 | Global native plugin, prompt admission hook | `~/.config/opencode/AGENTS.md` | MCP registered by plugin |
| Copilot CLI | `~/.copilot/hooks/tuna.json` | `~/.copilot/copilot-instructions.md` | `~/.copilot/mcp-config.json` |
| Codex | `~/.codex/hooks.json`, `UserPromptSubmit` | `~/.codex/AGENTS.md` | Managed section in `config.toml` |
| Claude Code | `~/.claude/settings.json`, `UserPromptSubmit` | `~/.claude/CLAUDE.md` | User-level `.claude.json` |

All instruction paths are symlinks to `~/.agents/AGENTS.md` by default. Setup preserves and merges existing instruction contents and keeps originals beside replaced files as `.tuna-backup-*`. Unrelated harness settings and hooks are retained. Existing harness policies, disabled hooks, or higher-priority instruction overrides can affect whether integrations run. Tuna honors `XDG_CONFIG_HOME`, `COPILOT_HOME`, `CODEX_HOME`, and `CLAUDE_CONFIG_DIR`.

MCP exposes `preferences`, `list`, and `remove`. For another MCP-capable client, register the stdio command `tuna mcp`. Agents can also call `tuna query --json`, `tuna list today --json`, and the other CLI commands directly. Hook capture is bounded, silent, and does nothing when tuna is stopped.

Integration references: [OpenCode V2 plugins](https://opencode.ai/v2/docs/build/plugins), [Copilot hooks](https://docs.github.com/en/copilot/reference/hooks-configuration), [Codex hooks](https://developers.openai.com/codex/hooks), [Claude hooks](https://code.claude.com/docs/en/hooks). Current hooks-capable versions are required; OpenCode V1 is not supported.

## Contributing

See the [contributor guide](CONTRIBUTING.md) for building, testing, and releasing tuna.
