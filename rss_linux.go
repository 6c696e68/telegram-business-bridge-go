//go:build linux

package main

import (
	"os"
	"strconv"
	"strings"
)

// processRSS reads the resident set size from /proc/self/statm (second field
// is resident pages). Returns 0 when unreadable.
func processRSS() uint64 {
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0
	}
	pages, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return pages * uint64(os.Getpagesize())
}
