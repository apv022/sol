package files

// watcher reports folder paths that may have changed.
type watcher interface {
	// Events delivers relative "/"-separated paths; "" means "scan everything".
	Events() <-chan string
	Close() error
}
