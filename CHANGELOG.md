# Changelog

All notable changes to the grailward agent are documented here. Binaries for every
release are published at [grailward.com/download](https://grailward.com/download)
(served from `downloads.grailward.com`, with SHA-256 checksums).

Versions up to v0.2.0 predate this public mirror, so they have no corresponding
source snapshot here; they are listed for completeness.

## v0.4.0 — 2026-07-10

- **Map exploration sync.** The fog-of-war "reveal" files that Diablo II:
  Resurrected keeps beside each character save (`<Name>.map` and `<Name>.ma0`…)
  now travel between your machines along with the character. A new tray toggle,
  **Sync map exploration** (on by default), governs the whole trail — turn it off
  and no map file is ever sent, downloaded, or written.
- The map files ride the character save: they are pushed right after a successful
  `.d2s` upload and pulled right after a `.d2s` is written in two-way mode. They
  are not a save — losing a slice only re-reveals a bit of map — so there is no
  conflict dialog: the server copy wins (last-writer-wins), and reveal files are
  never versioned. A push that fails is retried silently on the next scan and
  never holds up the save upload.
- Same safety discipline as saves for every map write: filenames from the server
  are strictly sanitized (only `<character>.map` / `<character>.ma<digit>`),
  payloads are SHA-256-verified, the previous file is backed up first, writes are
  atomic, and nothing is written while a game session looks active. `.key`/`.ctl`
  files stay out of scope entirely.

## v0.3.2 — 2026-07-08

- Embedded file icons: the Windows `.exe` now carries the grailward shield as a PE
  icon resource (plus version metadata in file properties), and the macOS `.app`
  bundle ships an `.icns` — no more generic file icons in Explorer/Finder.
- Tray menu regrouped: **Reset token** and **About Grailward Agent** share one
  section, and **Saves folder** became a submenu with **Open folder** and
  **Change folder** — the latter re-points the watcher at a different folder live,
  without restarting the app or re-running setup.

## v0.3.1 — 2026-07-08

- **Fixed: empty `agent.log` on Windows.** The log tee wrote to stderr first, and a
  windowed (no-console) Windows binary has a dead stderr handle, which starved the
  file writer — the log file stayed empty. The file now comes first and stderr is
  best-effort; the log is also armed before configuration loads, so setup errors
  are diagnosable on a GUI-only app.
- **Fixed: blank tray icon on Windows.** The Win32 tray requires ICO-format icons;
  the agent now wraps its PNG icons in an ICO container on Windows (macOS/Linux
  unchanged).
- New **About Grailward Agent** menu item: version, server, saves folder, sync
  mode, config/log location.
- The tray tooltip now shows the agent version.

## v0.3.0 — 2026-07-08

- **Two-way sync (opt-in).** For players who use more than one machine: when the
  server has a newer save (you played elsewhere), the agent offers to pull it and
  write it to your saves folder. Push-only remains the default and never writes.
- Every write requires explicit confirmation — offers appear at startup, when
  switching the mode to Two-way, and via the new **Pull latest now** menu item. A
  periodic check (~5 min) only signals (menu label shows a count); it never writes.
- Safety model: your current local file is guaranteed in the account history before
  any overwrite (in a conflict it is uploaded first, as a backup that does not
  become the server's current version); a local backup copy is kept; writes are
  atomic (temp file + rename) and SHA-256-verified against the server manifest;
  filenames from the server are strictly sanitized; the agent never deletes files.
- Conflict resolution dialog (**Keep local / Use server / Skip**) when both the
  local file and the server changed since the last sync — no version is ever lost.
- Best-effort game-detection guard: the agent refuses to write while Diablo II:
  Resurrected appears to be running or the saves folder shows recent activity.
- New protocol endpoints: `GET /api/v1/sync` (account manifest) and per-save
  current-version downloads; uploads gained an optional `set_current` flag for
  backup-only ingestion. See the README's protocol section.

## v0.2.1 — 2026-07-05

- Fixed `agent.log` spam: a per-file status line is logged only when it changes
  (a persistent condition logs once instead of flooding every scan).
- First version with this public source mirror.

## v0.2.0 — 2026-07-05

- The agent became a **menu-bar (macOS) / system-tray (Windows) app**: a grailward
  shield icon whose color reports state — gold = syncing, grey = paused, red =
  error — with a menu for pause/resume, poll interval, open saves folder, open
  logs, reset token, and quit.
- macOS distribution changed to a zipped `.app` bundle (background agent, no Dock
  icon).

## v0.1.1 — 2026-07-05

- Agent logs are canonical English regardless of the account's web language: the
  agent sends `Accept-Language: en` and the server honors it for API error strings.

## v0.1.0 — 2026-07-05

- First published release. Background watcher: scans the Diablo II: Resurrected
  saves folder, debounces writes, validates file headers, and uploads changed
  `.d2s`/`.d2i` files to grailward.com over a token-authenticated API. Native
  dialogs for first-run setup (API token, saves folder), `--version` flag,
  versioned `User-Agent`.
