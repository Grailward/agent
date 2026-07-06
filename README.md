# grailward-agent

The local **watcher** for [grailward.com](https://grailward.com): a tiny background app (Go)
that watches your Diablo II: Resurrected save folder and uploads every changed character
(`.d2s`) and shared stash (`.d2i`) to the platform. Read-only on your saves; no parsing on
the client — it just sends the raw bytes.

It runs as a **menu-bar app** (macOS) / **system-tray app** (Windows): a grailward shield
icon whose color reports state — **gold** = syncing, **grey** = paused, **red** = error —
with a menu for pause/resume, poll interval, open saves folder, open logs, reset token, quit.

> **This repository is a public, read-only mirror** of the agent's source from the grailward
> monorepo — published so you can read the exact code and build/run the agent yourself
> instead of trusting our (currently unsigned) binaries. Development happens upstream and is
> mirrored here; issues are welcome, but code changes land in the private monorepo.

## What it does — the whole protocol

Every couple of seconds the agent scans the save folder. For each `.d2s`/`.d2i` that changed
(debounced, so it only sends once the game has finished writing) and passes a cheap header
integrity check, it sends the raw bytes:

```
POST /api/v1/snapshots
Authorization: Bearer <your api token>
Content-Type: application/json

{ "raw_base64": "…", "sha256": "…", "source_machine": "…", "filename": "…" }
```

That's the entire client. No telemetry, no reading anything outside the save folder, and it
never writes to your saves.

## Layout

| File | What |
|---|---|
| `main.go` | Entry point: loads config, tees logs to `agent.log`, hands the main thread to the tray. |
| `tray.go` | Menu-bar UI (`fyne.io/systray`) + embedded icons; the watcher runs in a goroutine. |
| `watcher.go` | Poll loop, debounce, `.d2s`/`.d2i` validation, upload, per-file log de-dup. |
| `client.go` | HTTP client for `POST /api/v1/snapshots`. |
| `config.go` | Config load/save (`~/…/grailward-agent/config.json`), CLI flags, token helpers. |
| `platform_darwin.go` / `platform_windows.go` | Native dialogs (token / folder), open-path. |
| `icons/` | Tray icons (gold/grey/red), embedded via `go:embed`. |
| `build.sh` | Cross-platform build → `build/`. |

## Build it yourself

```sh
./build.sh          # produces a build in build/
```

Because the tray needs **CGO** (Cocoa on macOS, Win32 on Windows), the old pure-Go
`CGO_ENABLED=0` cross-compile no longer applies:

- **macOS** builds natively — a universal binary (`clang -arch`) wrapped in
  `Grailward Agent.app` (`LSUIElement=true` → menu-bar-only, no Dock, no terminal),
  then zipped to `grailward-agent-macos.zip` (the `.app` is a folder, so a single-file
  download has to be the zip).
- **Windows** requires a **mingw-w64** cross toolchain (`brew install mingw-w64`) and
  builds with `-H windowsgui` (no console window). Without mingw, `build.sh` skips the
  Windows target and prints a warning — it does not fail.

## Runtime config & CLI flags

Config lives at `~/Library/Application Support/grailward-agent/config.json` (macOS) /
`%AppData%\grailward-agent\config.json` (Windows); logs at `agent.log` beside it
(truncated each launch). The menu covers everyday use; the same binary also takes flags
from a terminal:

- `--version` — print version and exit.
- `--config` — re-open the setup dialogs (token / saves folder).
- `--clear-token` — erase the saved config and token.
- `--saves-dir <path>` — point at a specific save folder.
- `--poll <seconds>` — scan interval (default 2).

You get the **API token** from your account's *Devices* page on grailward.com; the agent
prompts for it (and for the saves folder) with a native dialog on first run.

## Known caveats

- **Unsigned / un-notarized.** Gatekeeper (macOS) and SmartScreen (Windows) warn on first
  open of the official downloads. Building from this source is exactly how you run a binary
  you fully control instead; signing + notarization is a paid Developer-ID step we haven't
  taken yet.
- **Windows builds need mingw locally** (or a CI runner).
- **No auto-update.** The release manifest leaves the door open, but an installed copy keeps
  running until it's manually replaced.

## License & affiliation

Released under the [MIT License](LICENSE).

grailward is a fan-made, unofficial tool for **single-player** Diablo II: Resurrected. It is
not affiliated with, endorsed by, or associated with Blizzard Entertainment. Diablo is a
trademark of Blizzard Entertainment, Inc.
