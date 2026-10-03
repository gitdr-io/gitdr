//go:build !linux

package main

// undumpable is a Linux protection (undumpable_linux.go). gitdr ships for Linux only, and runs
// anywhere else only in development, where no run's secrets are at stake.
func undumpable() error { return nil }
