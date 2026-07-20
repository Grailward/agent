package main

import "strings"

// Pure argument construction and result interpretation for the Linux conflict
// dialog, split out from platform_linux.go so the exact flags handed to kdialog and
// zenity — and the mapping from their (exit code, stdout) to a ConflictChoice — are
// unit-testable on any OS. Build-tag-free; unused off Linux, which Go allows.

// The three conflict choices share their button labels between kdialog and zenity so
// the wording never drifts. zenity additionally echoes the extra-button label on
// stdout, so conflictUseServerLabel doubles as the token interpretZenityConflict
// matches against.
const (
	conflictKeepLocalLabel = "Keep local"
	conflictUseServerLabel = "Use server"
	conflictSkipLabel      = "Skip"
)

// kdialogConflictArgs builds the kdialog three-button prompt: --yesnocancel maps Yes
// to "Keep local", No to "Use server", and Cancel to "Skip".
func kdialogConflictArgs(title, message string) []string {
	return []string{
		"--title", title,
		"--yesnocancel", message,
		"--yes-label", conflictKeepLocalLabel,
		"--no-label", conflictUseServerLabel,
		"--cancel-label", conflictSkipLabel,
	}
}

// interpretKdialogConflict maps a kdialog exit code to a choice: 0 = Yes (Keep
// local), 1 = No (Use server), 2 = Cancel (Skip). Anything unexpected is the
// conservative Skip.
func interpretKdialogConflict(exitCode int) ConflictChoice {
	switch exitCode {
	case 0:
		return ConflictKeepLocal
	case 1:
		return ConflictUseServer
	default: // 2 = Cancel, or any unexpected status
		return ConflictSkip
	}
}

// zenityConflictArgs builds the zenity three-button prompt. zenity's --question has
// only OK/Cancel, so the third choice is an --extra-button: OK becomes "Keep local",
// Cancel becomes "Skip", and the extra button is "Use server".
func zenityConflictArgs(title, message string) []string {
	return []string{
		"--question",
		"--title", title,
		"--text", message,
		"--ok-label", conflictKeepLocalLabel,
		"--cancel-label", conflictSkipLabel,
		"--extra-button", conflictUseServerLabel,
	}
}

// interpretZenityConflict maps a zenity result to a choice. zenity exits 0 for OK
// ("Keep local"); for an extra button it exits non-zero but echoes the button's
// label on stdout, so a stdout of "Use server" is checked first. Everything else —
// Cancel (non-zero, empty stdout), a closed window, an unknown status — is the
// conservative Skip.
func interpretZenityConflict(exitCode int, stdout string) ConflictChoice {
	if strings.TrimSpace(stdout) == conflictUseServerLabel {
		return ConflictUseServer
	}
	if exitCode == 0 {
		return ConflictKeepLocal
	}
	return ConflictSkip
}
