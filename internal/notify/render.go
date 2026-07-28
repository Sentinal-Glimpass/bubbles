package notify

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// ansiRe matches CSI/OSC-style escape sequences. Mapping ESC to a space (as
// the kernel's sanitizePTY does) would leave the "[31m" tail as literal
// garbage in the recipient's prompt, so the whole sequence is removed first.
var ansiRe = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]|\x1b[@-Z\\\\-_]")

// flatten collapses line structure to single spaces. It runs before sanitize
// because a multi-line string typed into a PTY is treated as a paste (and
// each newline as a submit), so line breaks are a delivery hazard, not just a
// formatting one.
func flatten(s string) string {
	return strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(s)
}

// sanitize strips escape sequences and control characters from text that gets
// TYPED into another bubble's terminal. Ported from the kernel's sanitizePTY:
// without it, a crafted subject could inject escape sequences or extra
// keystrokes into the recipient's session — one agent puppeting another.
// Bodies used to be exempt because they were only ever read via the inbox()
// tool, but inlining makes them typed input, so they are scrubbed too.
func sanitize(s string) string {
	s = ansiRe.ReplaceAllString(s, "")
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f { // C0 controls (incl. ESC, CR, LF, TAB) + DEL
			return ' '
		}
		return r
	}, s)
}

// renderNotice is the non-interrupting "you have mail" line: it announces
// only, and the content is read via inbox().
func renderNotice(source, subject string, unread int) string {
	return fmt.Sprintf("📬 New message from %s: %q — you have %d unread. Call the inbox() tool to read.",
		sanitize(flatten(source)), sanitize(flatten(subject)), unread)
}

// renderInline carries the body itself, saving the recipient an inbox()
// round-trip. body must already be sanitised by the caller.
func renderInline(source, subject, body string, unread int) string {
	return fmt.Sprintf("📬 New message from %s: %q — %s (%d unread; call inbox() for the rest.)",
		sanitize(flatten(source)), sanitize(flatten(subject)), body, unread)
}

// renderRollup summarises a batch that was never individually announced, so
// the recipient learns both the volume and how long it has been accumulating.
func renderRollup(n int, subject, source string, since time.Time) string {
	return fmt.Sprintf("📬 %d× %q from %s since %s — call inbox() to read.",
		n, sanitize(flatten(subject)), sanitize(flatten(source)), since.Format("15:04"))
}
