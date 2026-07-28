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

// Sanitize strips escape sequences and control characters from text that gets
// TYPED into another bubble's terminal. Without it, a crafted subject could
// inject escape sequences or extra keystrokes into the recipient's session —
// one agent puppeting another. Bodies used to be exempt because they were only
// ever read via the inbox() tool, but inlining makes them typed input, so they
// are scrubbed too.
//
// It is exported because the kernel types other text into sessions as well
// (slash commands with caller-supplied arguments), and every such path must
// use this one implementation rather than a weaker local copy: the kernel's
// old sanitizePTY mapped ESC to a space, which left the "[31m" tail of a
// sequence as literal garbage in the recipient's prompt. Whole sequences are
// removed here instead.
func Sanitize(s string) string {
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
		Sanitize(flatten(source)), Sanitize(flatten(subject)), unread)
}

// renderInline carries the body itself, saving the recipient an inbox()
// round-trip. body must already be sanitised by the caller.
func renderInline(source, subject, body string, unread int) string {
	return fmt.Sprintf("📬 New message from %s: %q — %s (%d unread; call inbox() for the rest.)",
		Sanitize(flatten(source)), Sanitize(flatten(subject)), body, unread)
}

// RenderDrain is the backlog line: it names no single sender because it stands
// for the whole queue. Exported because the operator paths (leaving a bubble,
// pausing typing) flush a held backlog with the same line, and every "you have
// mail" notice in the system must read identically to the recipient.
func RenderDrain(unread int) string {
	return fmt.Sprintf("📬 You have %d unread message(s) — call the inbox() tool to read and reply.", unread)
}

// renderRollup summarises a batch that was never individually announced, so
// the recipient learns both the volume and how long it has been accumulating.
func renderRollup(n int, subject, source string, since time.Time) string {
	return fmt.Sprintf("📬 %d× %q from %s since %s — call inbox() to read.",
		n, Sanitize(flatten(subject)), Sanitize(flatten(source)), since.Format("15:04"))
}
