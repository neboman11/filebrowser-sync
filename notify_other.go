//go:build !linux

package main

// notify is a no-op off Linux; the tray icon and menu still show the state.
func notify(summary, body string) {}
