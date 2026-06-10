package audit

import (
	"bufio"
	"os"
	"sync"
)

// WAL is a disk-backed, append-only buffer for audit records that a required
// sink failed to accept. Records survive a crash/restart and are replayed on
// reopen, so buffer_then_block never loses a record to an in-memory-only
// buffer (§5.4). Newline-delimited, same framing as the sinks.
type WAL struct {
	mu   sync.Mutex
	f    *os.File
	path string
}

func OpenWAL(path string) (*WAL, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &WAL{f: f, path: path}, nil
}

func (w *WAL) Append(rec []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.f.Write(rec); err != nil {
		return err
	}
	if _, err := w.f.Write([]byte{'\n'}); err != nil {
		return err
	}
	return w.f.Sync()
}

// Replay invokes fn for each buffered record in order.
func (w *WAL) Replay(fn func(rec []byte) error) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.f.Seek(0, 0); err != nil {
		return err
	}
	sc := bufio.NewScanner(w.f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		cp := append([]byte(nil), line...)
		if err := fn(cp); err != nil {
			return err
		}
	}
	return sc.Err()
}

// Truncate empties the WAL after its records have been durably flushed to sinks.
func (w *WAL) Truncate() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.f.Truncate(0); err != nil {
		return err
	}
	_, err := w.f.Seek(0, 0)
	return err
}

func (w *WAL) Close() error { return w.f.Close() }
