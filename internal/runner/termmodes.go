package runner

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

// termModes tracks the terminal modes a session's program has switched on, by
// watching its output, so a viewer that attaches later can be put into the same
// state. claude turns these on once, at startup — mouse reporting, bracketed
// paste, focus events, the kitty keyboard protocol — and never again. Leaving a
// dive switches them off (the fleet view needs a plain terminal), so without a
// replay every dive after the first silently lost mouse, paste and the richer
// key encodings while claude still believed they were on.
type termModes struct {
	dec   map[int]bool // DEC private modes (CSI ? n h / l) we track
	kitty []int        // kitty keyboard protocol flag stack (CSI > n u / CSI < u)
	mok   string       // modifyOtherKeys level from CSI > 4 ; n m ("" = off)
	tail  []byte       // an escape sequence split across output reads
}

// trackedDEC are the DEC private modes worth restoring for a viewer, in replay
// order. Alt-screen (1049) is deliberately absent: the hosted terminal is
// already on the alternate screen, and toggling it would wipe the frame.
var trackedDEC = []int{25, 1000, 1002, 1003, 1004, 1006, 1015, 2004, 2031}

// maxSeqLen bounds how much of an unterminated escape sequence is carried to
// the next read. Mode sequences are short; anything longer is not one of ours.
const maxSeqLen = 32

func isTrackedDEC(n int) bool {
	for _, m := range trackedDEC {
		if m == n {
			return true
		}
	}
	return false
}

// feed scans program output for mode changes.
func (m *termModes) feed(p []byte) {
	data := p
	if len(m.tail) > 0 {
		data = append(append([]byte{}, m.tail...), p...)
		m.tail = nil
	}
	for i := 0; i < len(data); {
		j := bytes.IndexByte(data[i:], 0x1b)
		if j < 0 {
			return
		}
		i += j
		n, complete := m.csi(data[i:])
		if !complete {
			if len(data)-i <= maxSeqLen {
				m.tail = append([]byte{}, data[i:]...)
			}
			return
		}
		i += n
	}
}

// csi parses one sequence at the start of b (which begins with ESC). It returns
// how many bytes it consumed and whether the sequence was complete.
func (m *termModes) csi(b []byte) (int, bool) {
	if len(b) < 2 {
		return 0, false
	}
	if b[1] != '[' {
		return 1, true // not CSI: skip the ESC
	}
	k := 2
	for k < len(b) && b[k] >= 0x20 && b[k] <= 0x3f { // parameter + intermediate bytes
		k++
		if k > maxSeqLen {
			return k, true // too long to be a mode sequence; not ours
		}
	}
	if k >= len(b) {
		return 0, false
	}
	final := b[k]
	if final < 0x40 || final > 0x7e {
		return k, true // malformed: resync past it
	}
	m.apply(string(b[2:k]), final)
	return k + 1, true
}

func (m *termModes) apply(params string, final byte) {
	switch {
	case strings.HasPrefix(params, "?") && (final == 'h' || final == 'l'):
		for _, f := range strings.Split(params[1:], ";") {
			if n, err := strconv.Atoi(f); err == nil && isTrackedDEC(n) {
				if m.dec == nil {
					m.dec = map[int]bool{}
				}
				m.dec[n] = final == 'h'
			}
		}
	case strings.HasPrefix(params, ">") && final == 'u': // kitty: push flags
		n, _ := strconv.Atoi(params[1:])
		m.kitty = append(m.kitty, n)
	case strings.HasPrefix(params, "<") && final == 'u': // kitty: pop n entries
		n := 1
		if v, err := strconv.Atoi(params[1:]); err == nil && v > 0 {
			n = v
		}
		if n > len(m.kitty) {
			n = len(m.kitty)
		}
		m.kitty = m.kitty[:len(m.kitty)-n]
	case strings.HasPrefix(params, "=") && final == 'u': // kitty: set current flags
		f := strings.SplitN(params[1:], ";", 2)
		flags, _ := strconv.Atoi(f[0])
		mode := 1
		if len(f) == 2 {
			mode, _ = strconv.Atoi(f[1])
		}
		cur := 0
		if len(m.kitty) > 0 {
			cur = m.kitty[len(m.kitty)-1]
		} else {
			m.kitty = []int{0}
		}
		switch mode {
		case 2:
			cur |= flags
		case 3:
			cur &^= flags
		default:
			cur = flags
		}
		m.kitty[len(m.kitty)-1] = cur
	case strings.HasPrefix(params, ">4") && final == 'm': // modifyOtherKeys
		if v := strings.TrimPrefix(strings.TrimPrefix(params, ">4"), ";"); v != "" && v != "0" {
			m.mok = v
		} else {
			m.mok = ""
		}
	}
}

// replay returns the sequences that put a fresh terminal into the program's
// current mode state. The cursor is always stated (claude hides it during
// redraws, and the fleet view leaves it hidden).
func (m *termModes) replay() []byte {
	var b bytes.Buffer
	for _, n := range trackedDEC {
		on, seen := m.dec[n]
		switch {
		case n == 25:
			if !seen || on {
				b.WriteString("\x1b[?25h")
			} else {
				b.WriteString("\x1b[?25l")
			}
		case on:
			fmt.Fprintf(&b, "\x1b[?%dh", n)
		}
	}
	if len(m.kitty) > 0 && m.kitty[len(m.kitty)-1] != 0 {
		fmt.Fprintf(&b, "\x1b[>%du", m.kitty[len(m.kitty)-1])
	}
	if m.mok != "" {
		fmt.Fprintf(&b, "\x1b[>4;%sm", m.mok)
	}
	return b.Bytes()
}
