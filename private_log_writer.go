package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/keakon/golog"
)

type privateRotatingLogWriter struct {
	mu      sync.Mutex
	writer  io.WriteCloser
	path    string
	maxSize uint64
	pos     uint64
}

func newPrivateRotatingLogWriter(path string, maxSize uint64, backupCount uint8) (*privateRotatingLogWriter, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, privateDirMode); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, privateFileMode)
	if err != nil {
		return nil, fmt.Errorf("create log file: %w", err)
	}
	info, statErr := f.Stat()
	closeErr := f.Close()
	if statErr != nil {
		return nil, fmt.Errorf("stat log file: %w", statErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close log file: %w", closeErr)
	}
	if err := os.Chmod(path, privateFileMode); err != nil {
		return nil, fmt.Errorf("restrict log file permissions: %w", err)
	}

	writer, err := golog.NewRotatingFileWriter(path, maxSize, backupCount)
	if err != nil {
		return nil, err
	}
	return &privateRotatingLogWriter{
		writer:  writer,
		path:    path,
		maxSize: maxSize,
		pos:     uint64(info.Size()),
	}, nil
}

func (w *privateRotatingLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	n, err := w.writer.Write(p)
	w.pos += uint64(n)
	if w.pos >= w.maxSize {
		w.pos = 0
		if chmodErr := os.Chmod(w.path, privateFileMode); err == nil && chmodErr != nil {
			err = fmt.Errorf("restrict rotated log file permissions: %w", chmodErr)
		}
	}
	return n, err
}

func (w *privateRotatingLogWriter) Close() error {
	return w.writer.Close()
}
