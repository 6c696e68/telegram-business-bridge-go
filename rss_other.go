//go:build !linux

package main

import "runtime"

// processRSS approximates RSS with runtime.Sys on non-Linux platforms.
func processRSS() uint64 {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.Sys
}
