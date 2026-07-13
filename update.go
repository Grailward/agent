package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Self-update. The check is a separate concern from save sync: it fetches the
// public release manifest, compares its semver with the embedded build version,
// and — only when strictly newer — surfaces a "Update to vX.Y.Z…" item in the
// tray. Nothing is downloaded or written without an explicit confirmation dialog.
//
// A check failure is silent on the tray (log-only, deduplicated) and NEVER enters
// the red sync-error latch: an update problem is not a save problem. The apply
// step verifies the artifact's SHA-256 before touching anything and refuses to run
// while a game session looks active.

const (
	// defaultUpdateManifestURL is the public "latest" release manifest; it always
	// names the newest published version.
	defaultUpdateManifestURL = "https://downloads.grailward.com/agent/latest/manifest.json"

	// updateCheckInterval is the periodic re-check cadence (in addition to a check
	// at startup).
	updateCheckInterval = 6 * time.Hour

	// updateLogKey deduplicates update log lines (log-only, no latch).
	updateLogKey = "__update__"

	manifestFetchTimeout    = 20 * time.Second
	artifactDownloadTimeout = 10 * time.Minute
	maxManifestBytes        = 1 << 20  // 1 MiB — the manifest is tiny
	maxArtifactBytes        = 64 << 20 // 64 MiB — generous cap for the binary/zip
)

// updateUIState is the tray's view of the update offer.
type updateUIState int

const (
	updateNone      updateUIState = iota // no newer version — hide the item
	updateAvailable                      // a newer version is offered
	updateFailed                         // the last apply failed — show "see log"
)

// UpdateManifest mirrors the published release manifest served at
// agent/latest/manifest.json on the downloads host.
type UpdateManifest struct {
	Version    string       `json:"version"`
	ReleasedAt string       `json:"released_at"`
	Files      []UpdateFile `json:"files"`
}

// UpdateFile is one platform artifact in the manifest. On macOS "name" is the
// zipped .app (grailward-agent-macos.zip); on Windows it is the raw .exe.
type UpdateFile struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	URL    string `json:"url"`
}

// parseUpdateManifest decodes and minimally validates a manifest. Pure, so the
// parse is unit-testable.
func parseUpdateManifest(data []byte) (*UpdateManifest, error) {
	var m UpdateManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	if strings.TrimSpace(m.Version) == "" {
		return nil, errors.New("manifest missing version")
	}
	if len(m.Files) == 0 {
		return nil, errors.New("manifest has no files")
	}
	return &m, nil
}

// selectUpdateFile picks the artifact for a platform. The darwin build is a
// universal binary, so any darwin entry matches regardless of goarch; other OSes
// match on arch.
func selectUpdateFile(m *UpdateManifest, goos, goarch string) (*UpdateFile, bool) {
	for i := range m.Files {
		f := &m.Files[i]
		if f.OS != goos {
			continue
		}
		if goos == "darwin" || f.Arch == goarch {
			return f, true
		}
	}
	return nil, false
}

// semver is a parsed version: major.minor.patch, an optional fourth numeric
// build component (release.sh permits v0.5.0.1), and an optional prerelease
// identifier (the "-rc1" tail, without its leading dash).
type semver struct {
	maj, min, pat, build int
	pre                  string
}

// semverRe captures major.minor.patch, an optional numeric 4th component, and the
// remainder (a prerelease "-rc1" or any leftover suffix).
var semverRe = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)(?:\.(\d+))?(.*)$`)

// parseSemver parses a version. Returns ok=false for anything unparseable —
// notably "dev", so a local dev build never self-updates.
func parseSemver(s string) (v semver, ok bool) {
	m := semverRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return semver{}, false
	}
	v.maj, _ = strconv.Atoi(m[1])
	v.min, _ = strconv.Atoi(m[2])
	v.pat, _ = strconv.Atoi(m[3])
	if m[4] != "" {
		v.build, _ = strconv.Atoi(m[4])
	}
	v.pre = strings.TrimPrefix(m[5], "-") // drop the dash that introduces a prerelease
	return v, true
}

// compareSemver returns (-1|0|1, true) ordering a against b, or (0, false) when
// either side is unparseable. It compares major, minor, patch, then the numeric
// 4th component (absent = 0, so v0.5.0.1 > v0.5.0), then the prerelease tail.
func compareSemver(a, b string) (int, bool) {
	av, aok := parseSemver(a)
	bv, bok := parseSemver(b)
	if !aok || !bok {
		return 0, false
	}
	for _, c := range []int{
		cmpInt(av.maj, bv.maj),
		cmpInt(av.min, bv.min),
		cmpInt(av.pat, bv.pat),
		cmpInt(av.build, bv.build),
	} {
		if c != 0 {
			return c, true
		}
	}
	return comparePre(av.pre, bv.pre), true
}

// comparePre orders two prerelease tails. A full release (empty tail) outranks any
// prerelease. Two prereleases sharing an alphabetic prefix compare their trailing
// number numerically (so rc10 > rc2); otherwise they compare lexically.
func comparePre(a, b string) int {
	if a == b {
		return 0
	}
	if a == "" { // a is a full release, b is a prerelease of the same core
		return 1
	}
	if b == "" {
		return -1
	}
	aprefix, anum, aok := splitPre(a)
	bprefix, bnum, bok := splitPre(b)
	if aok && bok && aprefix == bprefix {
		return cmpInt(anum, bnum)
	}
	return strings.Compare(a, b)
}

// splitPre separates a prerelease identifier into its alphabetic prefix and its
// trailing number: "rc10" -> ("rc", 10, true), "5" -> ("", 5, true). A value with
// no trailing digits yields (s, 0, false).
func splitPre(s string) (prefix string, num int, hasNum bool) {
	i := len(s)
	for i > 0 && s[i-1] >= '0' && s[i-1] <= '9' {
		i--
	}
	if i == len(s) {
		return s, 0, false
	}
	n, err := strconv.Atoi(s[i:])
	if err != nil {
		return s, 0, false
	}
	return s[:i], n, true
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// isNewer reports whether remote is a strictly newer, parseable version than
// current. An unparseable current (e.g. "dev") is never eligible for an update.
func isNewer(remote, current string) bool {
	c, ok := compareSemver(remote, current)
	return ok && c > 0
}

// checkSHA256 verifies data against a hex digest (case-insensitive).
func checkSHA256(data []byte, want string) error {
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("sha256 mismatch: got %s, want %s", got, want)
	}
	return nil
}

// macUpdateTarget reports whether execPath is the binary inside a .app bundle and,
// if so, returns the bundle path (…/Name.app). Pure string logic (the per-platform
// apply plan), so it is unit-testable on any host.
func macUpdateTarget(execPath string) (bundlePath string, isBundle bool) {
	macosDir := filepath.Dir(execPath)    // …/Name.app/Contents/MacOS
	contentsDir := filepath.Dir(macosDir) // …/Name.app/Contents
	bundle := filepath.Dir(contentsDir)   // …/Name.app
	if filepath.Base(macosDir) == "MacOS" &&
		filepath.Base(contentsDir) == "Contents" &&
		strings.HasSuffix(bundle, ".app") {
		return bundle, true
	}
	return "", false
}

// fetchUpdateManifest GETs and parses the manifest with a short timeout.
func fetchUpdateManifest(url string, client *http.Client) (*UpdateManifest, error) {
	ctx, cancel := context.WithTimeout(context.Background(), manifestFetchTimeout)
	defer cancel()
	body, err := httpGet(ctx, url, client, maxManifestBytes)
	if err != nil {
		return nil, err
	}
	return parseUpdateManifest(body)
}

// downloadArtifact GETs a release artifact with a generous timeout.
func downloadArtifact(url string, client *http.Client) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), artifactDownloadTimeout)
	defer cancel()
	return httpGet(ctx, url, client, maxArtifactBytes)
}

func httpGet(ctx context.Context, url string, client *http.Client, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "grailward-agent/"+Version)
	req.Header.Set("Accept-Language", "en")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// SetUpdateFunc registers the callback used to reflect the update-offer state onto
// the tray menu item.
func (w *Watcher) SetUpdateFunc(f func(state updateUIState, version string)) {
	w.mu.Lock()
	w.onUpdate = f
	w.mu.Unlock()
}

// RequestUpdate asks the scan goroutine to run the interactive update. A
// non-blocking nudge (deduplicated by the buffered channel), mirroring RequestPull.
func (w *Watcher) RequestUpdate() {
	select {
	case w.updateReq <- struct{}{}:
	default:
	}
}

func (w *Watcher) manifestURL() string {
	if w.updateURL != "" {
		return w.updateURL
	}
	return defaultUpdateManifestURL
}

func (w *Watcher) updateClient() *http.Client {
	if w.updateHTTP != nil {
		return w.updateHTTP
	}
	return http.DefaultClient
}

func (w *Watcher) buildVersion() string {
	if w.currentVersion != "" {
		return w.currentVersion
	}
	return Version
}

// checkForUpdate fetches the manifest and offers an update when it is strictly
// newer. Runs on the scan goroutine. Failures are log-only and deduplicated; they
// never touch the sync-error latch (an update problem is not a save problem).
func (w *Watcher) checkForUpdate() {
	// A build whose version isn't semver (a local "dev" build) can never be newer
	// than a release, so skip the network round-trip entirely.
	if _, ok := parseSemver(w.buildVersion()); !ok {
		return
	}
	m, err := fetchUpdateManifest(w.manifestURL(), w.updateClient())
	if err != nil {
		w.logOnce(updateLogKey, "Update check failed: "+err.Error())
		return
	}
	file, ok := selectUpdateFile(m, runtime.GOOS, runtime.GOARCH)
	if !ok {
		w.logOnce(updateLogKey, "Update manifest lists no artifact for this platform")
		return
	}
	if !isNewer(m.Version, w.buildVersion()) {
		w.clearUpdateOffer()
		return
	}
	w.setUpdateOffer(m.Version, file)
	w.logOnce(updateLogKey, "Update available: "+m.Version)
}

// setUpdateOffer stores a newer-version offer and shows the tray item. A
// successful check always resets a prior "failed" state back to a live offer.
func (w *Watcher) setUpdateOffer(version string, file *UpdateFile) {
	w.mu.Lock()
	changed := w.updateState != updateAvailable || w.updateOfferVer != version
	w.updateOffer = file
	w.updateOfferVer = version
	w.updateState = updateAvailable
	f := w.onUpdate
	w.mu.Unlock()
	if changed && f != nil {
		f(updateAvailable, version)
	}
}

// clearUpdateOffer hides the tray item when there is nothing newer to offer.
func (w *Watcher) clearUpdateOffer() {
	w.mu.Lock()
	changed := w.updateState != updateNone
	w.updateOffer = nil
	w.updateOfferVer = ""
	w.updateState = updateNone
	f := w.onUpdate
	w.mu.Unlock()
	if changed && f != nil {
		f(updateNone, "")
	}
}

// markUpdateFailed flips the tray item to "Update failed — see log" while keeping
// the offer, so the next successful check (or a retry click) can recover it.
func (w *Watcher) markUpdateFailed() {
	w.mu.Lock()
	w.updateState = updateFailed
	version := w.updateOfferVer
	f := w.onUpdate
	w.mu.Unlock()
	if f != nil {
		f(updateFailed, version)
	}
}

// currentUpdateOffer snapshots the offered file+version under the lock.
func (w *Watcher) currentUpdateOffer() (*UpdateFile, string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.updateOffer, w.updateOfferVer
}

// runUpdate performs the interactive update on the scan goroutine: confirm →
// game-open guard → download → verify SHA-256 → platform apply (swap + restart).
// Being on the scan goroutine means no save transfer is ever in flight here.
func (w *Watcher) runUpdate() {
	w.ensureSeams()
	file, version := w.currentUpdateOffer()
	if file == nil {
		return
	}

	ok, err := w.confirmUpdate(version)
	if err != nil {
		log.Printf("Update confirmation dialog failed: %v", err)
		return
	}
	if !ok {
		w.logOnce(updateLogKey, "Update to "+version+" postponed by user")
		return
	}

	// Never apply while a game session looks active — refuse clearly and keep the
	// menu item so the user can retry once the game is closed.
	if running, reason := w.gameRunning(w.SavesDir(), time.Now()); running {
		w.deferUpdateForGame(reason)
		return
	}

	log.Printf("Downloading update %s (%s)…", version, file.Name)
	data, err := downloadArtifact(file.URL, w.updateClient())
	if err != nil {
		w.logOnce(updateLogKey, "Update download failed: "+err.Error())
		w.markUpdateFailed()
		return
	}
	if err := checkSHA256(data, file.SHA256); err != nil {
		w.logOnce(updateLogKey, "Update checksum mismatch for "+version+": "+err.Error())
		w.markUpdateFailed()
		return
	}

	exe, err := currentExecPath()
	if err != nil {
		w.logOnce(updateLogKey, "Update aborted: cannot resolve executable path: "+err.Error())
		w.markUpdateFailed()
		return
	}

	// Re-check the game guard immediately before the swap: the download took time,
	// and a session may have started meanwhile (TOCTOU). Defer as above — keep the
	// offer, apply nothing.
	if running, reason := w.gameRunning(w.SavesDir(), time.Now()); running {
		w.deferUpdateForGame(reason)
		return
	}

	log.Printf("Applying update %s…", version)
	if err := w.applyUpdate(exe, file, data); err != nil {
		w.logOnce(updateLogKey, "Update apply failed: "+err.Error())
		w.markUpdateFailed()
		return
	}
	// On success the platform apply restarts the process and does not return.
}

// deferUpdateForGame refuses an update while a game session looks active: it logs,
// shows a clear "close the game" dialog, and leaves the offer in place so the user
// can retry. Nothing is applied.
func (w *Watcher) deferUpdateForGame(reason string) {
	log.Printf("Update deferred: %s.", reason)
	notify := w.showMessage
	if notify == nil {
		notify = ShowAbout
	}
	_ = notify("Grailward can't update while Diablo II: Resurrected is running.\n\nClose the game, then choose Update again.")
}
