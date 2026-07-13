# grailward-agent

The local **watcher** for [grailward.com](https://grailward.com): a tiny background app (Go)
that watches your Diablo II: Resurrected save folder and uploads every changed character
(`.d2s`) and shared stash (`.d2i`) to the platform. No parsing on the client — it just sends
the raw bytes.

By default (**push-only**) it never writes to your saves at all: it only reads and uploads.
An opt-in **two-way** mode can additionally pull newer saves from the server back onto disk —
but only after you explicitly confirm each write. See [Sync modes](#sync-modes) below.

It runs as a **menu-bar app** (macOS) / **system-tray app** (Windows): a grailward shield
icon whose color reports state — **gold** = syncing, **grey** = paused, **red** = error —
with a menu for pause/resume, sync mode, sync map exploration (on/off), pull latest now,
poll interval, a Saves folder submenu (open the watched folder or change to a different
one), open logs, about (version and configuration details), reset token, quit.

It also syncs each character's **map exploration** (the fog-of-war "reveal" files
D2R keeps beside the save) across machines — see [Map exploration sync](#map-exploration-sync).

> **This repository is a public, read-only mirror** of the agent's source from the grailward
> monorepo — published so you can read the exact code and build/run the agent yourself
> instead of trusting our (currently unsigned) binaries. Development happens upstream and is
> mirrored here.
>
> **Contributing:** issues — bug reports and ideas — are very welcome. Because this is a mirror,
> its tree is overwritten on every sync, so PRs can't be merged here directly; but a fix you
> propose (in an issue or a PR) gets re-applied upstream in the monorepo, with credit.

## What it does — the whole protocol

Every couple of seconds the agent scans the save folder. For each `.d2s`/`.d2i` that changed
(debounced, so it only sends once the game has finished writing) and passes a cheap header
integrity check, it uploads the raw bytes:

```
POST /api/v1/snapshots
Authorization: Bearer <your api token>
Content-Type: application/json

{ "raw_base64": "…", "sha256": "…", "source_machine": "…", "filename": "…" }
```

Uploads are always accepted. `set_current` is an optional field — omitted, the snapshot
becomes the current save for its slot; sent as `false`, it is stored as a non-current backup
(used only when resolving a two-way conflict in the server's favor, see below).

**Two-way mode** adds two read endpoints. First a manifest of what the server holds:

```
GET /api/v1/sync
Authorization: Bearer <your api token>

{ "characters":     [ { "filename": "…", "sha256": "…", "download_path": "…", … } ],
  "shared_stashes": [ { "filename": "…", "sha256": "…", "download_path": "…", … } ] }
```

Then, for a save the agent decides to pull, the raw bytes at the server-provided
`download_path` (used verbatim, never re-constructed):

```
GET <download_path>
Authorization: Bearer <your api token>

200 application/octet-stream   (body = raw save bytes; X-Sha256 header = expected digest)
```

If the server has pull turned off it answers the manifest with `503 {"error": …}`; the agent
degrades quietly (a status message, no crash, no aggressive retry). That is the entire client:
no telemetry, and nothing outside the save folder is ever read.

## Sync modes

The mode is per-device, stored as `sync_mode` in `config.json`, and switchable live from the
tray menu.

- **`push` (default).** Upload-only. The agent **never** writes to your saves — it only reads
  and sends changed files. This is the original behavior.
- **`two_way` (opt-in).** In addition to uploading, the agent offers to pull newer server
  saves onto disk. Every write is opt-in and confirmed by you; nothing is ever written
  automatically or silently. On app start, whenever you pick **Pull latest now**, and the
  moment you switch to two-way from the tray menu, it fetches the manifest and, if the
  server is ahead:
  - one batch confirmation for **fast-forwards / new saves** — cases where your local bytes
    are already in the server's history (or there is no local file at all), so there is
    nothing to lose;
  - one three-way prompt **per conflicting file** — *Keep local* (upload yours as current),
    *Use server* (back yours up to the server first, then download the server's copy), or
    *Skip*.

  A background check every 5 minutes only **signals** (updates the tray tooltip and the
  "Pull latest now" label with a count) — it never opens a dialog and never writes.

Before **any** write, and only ever as a reinforcement on top of your confirmation, the agent
refuses if it detects a running game session. Every write also: backs up the file it is about
to overwrite (to a `backups/` folder beside the config), verifies the downloaded bytes'
sha256 against both the manifest and the `X-Sha256` header, and writes atomically (temp file
+ rename in the same folder). It never deletes a local save and never writes outside the
saves folder. For the *Use server* conflict path, your local bytes are guaranteed onto the
server before anything is overwritten (fast-forwards need no such upload — they are already
in the server's history by definition).

## Map exploration sync

D2R stores each character's explored map (fog of war) in sidecar files that share the
save's base name: `<Name>.map` and `<Name>.ma0`…`.ma4`. The map layout derives from a
seed inside the `.d2s`, so carrying these files across machines keeps your revealed map in
step with the character. The **Sync map exploration** tray toggle (on by default) governs
the whole trail; turn it off and no map file is sent, downloaded, or written.

Reveal files are not a save — losing a slice only re-hides a bit of map — so there is no
conflict resolution: they simply follow the `.d2s`, **last-writer-wins**. They are pushed
right after a successful character upload and pulled right after a character is written in
two-way mode (and, silently, when only the map differs while the save is already in sync).
The `.key`/`.ctl` files are out of scope. Every map write reuses the same safety rules as a
save: server filenames are strictly sanitized to `<character>.map` / `<character>.ma<digit>`,
payloads are SHA-256-verified against both the manifest and the batch body, the previous
file is backed up first, the write is atomic, and nothing is written while a game session
looks active. Two endpoints back it:

```
PUT /api/v1/characters/<name>/sidecars      (batch upload; idempotent)
{ "source_machine": "…",
  "files": [ { "filename": "<name>.map", "sha256": "…", "raw_base64": "…" }, … ] }

GET /api/v1/characters/<name>/sidecars       (batch download, path from the manifest)
{ "files": [ { "filename": "…", "sha256": "…", "raw_base64": "…" }, … ] }
```

Each character entry in `GET /api/v1/sync` also lists its `sidecars`
(`filename`/`sha256`/`size`) and a `sidecars_download_path`, so the agent knows which map
files differ before fetching anything.

## Layout

| File | What |
|---|---|
| `main.go` | Entry point: loads config, tees logs to `agent.log`, hands the main thread to the tray. |
| `tray.go` | Menu-bar UI (`fyne.io/systray`) + embedded icons; the watcher runs in a goroutine. |
| `watcher.go` | Poll loop, debounce, `.d2s`/`.d2i` validation, upload, per-file log de-dup, pull scheduling. |
| `pull.go` | Two-way orchestration: manifest evaluation, confirmations, downloads, conflict resolution. |
| `sync.go` | Manifest types, the decision matrix, filename sanitization, atomic verified write + backup. |
| `sync_state.go` | Persistent per-file "last confirmed sync" sha (`sync_state.json`). |
| `client.go` | HTTP client for `POST /api/v1/snapshots`, `GET /api/v1/sync`, and downloads. |
| `config.go` | Config load/save (`~/…/grailward-agent/config.json`), CLI flags, token helpers. |
| `platform_darwin.go` / `platform_windows.go` | Native dialogs (token / folder / confirm / conflict), game-running check, open-path. |
| `icons/` | Tray icons (gold/grey/red), embedded via `go:embed`, plus `app.png` (the file/bundle icon art). |
| `tools/mkico/` | Build-time helper: packs PNGs into a multi-size `.ico` for the Windows resource. |
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
  builds with `-H windowsgui` (no console window). The app icon and version metadata are
  embedded as a PE resource: `mkico` (`tools/mkico`) packs the icon into a multi-size
  `.ico`, and `windres` compiles it into the `.exe`. Without mingw, `build.sh` skips the
  Windows target and prints a warning — it does not fail; without `windres` it builds a
  resource-less `.exe`.

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
- **Updates are offered, never forced.** The agent checks the release manifest in the
  background (on start and every ~6 h); when a newer version exists, an "Update to vX.Y.Z…"
  item appears in the tray menu. Nothing is downloaded or replaced until you click it and
  confirm — the new binary is SHA-256-verified against the manifest before the swap, the
  previous copy is kept as a one-level backup, and the agent relaunches itself. No update
  is ever applied while a transfer is in flight or the game looks open.

## License & affiliation

Released under the [MIT License](LICENSE).

grailward is a fan-made, unofficial tool for **single-player** Diablo II: Resurrected. It is
not affiliated with, endorsed by, or associated with Blizzard Entertainment. Diablo is a
trademark of Blizzard Entertainment, Inc.
