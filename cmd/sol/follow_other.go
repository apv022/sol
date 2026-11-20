//go:build !linux

package main

import "context"

// waitGrow waits for more log output; without inotify it sleeps briefly at EOF, which is
// acceptable for tailing one local file. It returns false when ctx ended.
func waitGrow(ctx context.Context, _ string, _ int64) bool { return sleepGrow(ctx) }
