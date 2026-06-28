// Package addr defines hierarchical bubble addresses like "0", "0.1", "0.1.2".
package addr

import (
	"errors"
	"strings"
)

// Address is an immutable hierarchical address. Compare with ==.
type Address string

// Root is the human/operator address.
const Root Address = "0"

// ErrInvalid is returned by Parse for malformed addresses.
var ErrInvalid = errors.New("addr: invalid address")

// Parse validates s: "0" optionally followed by ".N" segments, each non-empty.
func Parse(s string) (Address, error) {
	if s == "" {
		return "", ErrInvalid
	}
	parts := strings.Split(s, ".")
	if parts[0] != "0" {
		return "", ErrInvalid
	}
	for _, p := range parts {
		if p == "" {
			return "", ErrInvalid
		}
	}
	return Address(s), nil
}

func (a Address) String() string { return string(a) }

// Child returns a new address with seg appended.
func (a Address) Child(seg string) Address { return Address(string(a) + "." + seg) }

// Parent returns the parent and true, or ("", false) for root.
func (a Address) Parent() (Address, bool) {
	i := strings.LastIndex(string(a), ".")
	if i < 0 {
		return "", false
	}
	return a[:i], true
}

// IsRoot reports whether a is the root address.
func (a Address) IsRoot() bool { return a == Root }
