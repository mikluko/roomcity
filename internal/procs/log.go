package procs

import (
	"os"
	"sync"
)

// DefaultMaxLog is the size at which a process's log is cut when the
// Manager names none.
const DefaultMaxLog = 10 << 20

// capped is the log file of a process: when a write would take it past max,
// the file is moved to <path>.1, replacing the one that was, and a new one
// is begun. A process's log is so held to twice max.
type capped struct {
	path string
	max  int64

	mu   sync.Mutex
	f    *os.File
	size int64
}

func openCapped(path string, max int64) (*capped, error) {
	c := &capped{path: path, max: max}
	if err := c.open(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *capped) open() error {
	f, err := os.OpenFile(c.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	c.f, c.size = f, info.Size()
	return nil
}

func (c *capped) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.size > 0 && c.size+int64(len(b)) > c.max {
		if err := c.f.Close(); err != nil {
			return 0, err
		}
		if err := os.Rename(c.path, c.path+".1"); err != nil {
			return 0, err
		}
		if err := c.open(); err != nil {
			return 0, err
		}
	}
	n, err := c.f.Write(b)
	c.size += int64(n)
	return n, err
}

func (c *capped) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.f.Close()
}
