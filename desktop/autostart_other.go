//go:build !windows

package main

import "errors"

func autostartEnabled() bool { return false }

func setAutostart(bool) error { return errors.New("start at login is only implemented on Windows") }
