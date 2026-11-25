//go:build !linux

package main

// sweepScopes: only Linux keeps process runners in systemd scopes.
func sweepScopes() int { return 0 }
