package main

import "strings"

// Platform-independent core of the Linux "Start with system" feature: the XDG
// autostart .desktop entry is rendered, parsed, and compared here as pure string
// logic so it is unit-testable on any OS. The side-effecting wrapper (writing
// ~/.config/autostart/grailward-agent.desktop and reading it back) lives in
// platform_linux.go. Build-tag-free means these helpers compile everywhere and are
// simply unused off Linux, which Go allows.

// autostartName and autostartComment are the fixed presentation strings of the
// autostart entry. Everything except the Exec target is constant, so the rendered
// body is fully determined by execPath — which is what makes the byte-for-byte
// up-to-date check meaningful.
const (
	autostartName    = "Grailward Agent"
	autostartComment = "Automatic backup for your Diablo II: Resurrected saves"
)

// autostartDesktopEntry renders the XDG autostart .desktop file that launches the
// agent at login. The body is deterministic in execPath: same path in, same bytes
// out, so autostartUpToDate can detect an entry written by an older build and
// rewrite it. X-GNOME-Autostart-enabled keeps GNOME from silently disabling the
// entry; KDE Plasma (the Steam Deck's desktop) honours the file directly.
func autostartDesktopEntry(execPath string) string {
	return "[Desktop Entry]\n" +
		"Type=Application\n" +
		"Name=" + autostartName + "\n" +
		"Comment=" + autostartComment + "\n" +
		"Exec=" + desktopExecQuote(execPath) + "\n" +
		"X-GNOME-Autostart-enabled=true\n"
}

// autostartUpToDate reports whether an existing .desktop body is exactly what this
// build would write for execPath, mirroring launchAgentUpToDate on macOS. A file
// from an older version therefore reads as out of date and is rewritten by the
// startup self-heal. Pure (no I/O) so the decision is unit-testable.
func autostartUpToDate(content, execPath string) bool {
	return content == autostartDesktopEntry(execPath)
}

// parseAutostartExec extracts the executable path from the Exec= line of a .desktop
// body, undoing the desktop-entry quoting/escaping. Returns ("", false) when there
// is no Exec= line. Pure (no file I/O) so it is unit-testable.
func parseAutostartExec(content string) (string, bool) {
	for _, line := range strings.Split(content, "\n") {
		if v, ok := strings.CutPrefix(line, "Exec="); ok {
			return parseDesktopExecValue(v), true
		}
	}
	return "", false
}

// desktopExecQuote renders execPath as a single quoted argument for the Exec key,
// per the freedesktop Desktop Entry spec. Two escaping layers apply, in the order a
// reader undoes them: the argument-quoting layer escapes the reserved characters
// (double quote, backtick, dollar, backslash) with a leading backslash, and the
// value-string layer then doubles every backslash. The whole thing is always
// wrapped in double quotes so a path with spaces parses as one argument.
func desktopExecQuote(execPath string) string {
	return `"` + desktopValueEscape(desktopArgEscape(execPath)) + `"`
}

// desktopArgEscape applies the Exec argument-quoting layer: each reserved character
// is prefixed with a backslash.
func desktopArgEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"', '`', '$', '\\':
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// desktopValueEscape applies the desktop-entry string-value layer: a literal
// backslash is written as two. It runs after desktopArgEscape, so the backslashes
// that layer introduced are doubled too — which is why a literal backslash in the
// path ends up as four in the file, exactly as the spec requires.
func desktopValueEscape(s string) string {
	return strings.ReplaceAll(s, `\`, `\\`)
}

// parseDesktopExecValue reverses desktopExecQuote for a single Exec argument: undo
// the value-string layer, strip the surrounding quotes, then undo the
// argument-quoting layer. Tolerant of an unquoted value (returned as-is).
func parseDesktopExecValue(raw string) string {
	s := desktopValueUnescape(strings.TrimSpace(raw))
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = desktopArgUnescape(s[1 : len(s)-1])
	}
	return s
}

// desktopValueUnescape reverses the desktop-entry string-value escapes. The agent
// only ever emits doubled backslashes, but the full standard set (\s \n \t \r \\)
// is handled so a hand-edited file still parses sanely.
func desktopValueUnescape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case 's':
				b.WriteByte(' ')
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case '\\':
				b.WriteByte('\\')
			default:
				b.WriteByte('\\')
				b.WriteByte(s[i+1])
			}
			i++
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// desktopArgUnescape reverses the argument-quoting layer inside a quoted argument:
// a backslash escapes the following character (which is emitted literally).
func desktopArgUnescape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			b.WriteByte(s[i+1])
			i++
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
