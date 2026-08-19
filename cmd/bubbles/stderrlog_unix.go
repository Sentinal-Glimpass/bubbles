//go:build unix

package main

import "golang.org/x/sys/unix"

// dupTo makes newfd a copy of oldfd, closing whatever newfd was.
//
// unix.Dup3 rather than syscall.Dup2: Dup2 does not exist on arm64 Linux, where
// only dup3 is wired up. A zero flags argument makes Dup3 behave as Dup2 (in
// particular it does NOT set close-on-exec, which matters — child processes are
// meant to inherit this descriptor).
func dupTo(oldfd, newfd int) error { return unix.Dup3(oldfd, newfd, 0) }
