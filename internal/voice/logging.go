package voice

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

const (
	debugLogPath    = "/tmp/yandex-station-chatgpt-voice.debug.log"
	debugLogMax     = 256 << 10
	debugLogBackups = 2
)

type rotatingLog struct {
	mu   sync.Mutex
	path string
	max  int64
	file *os.File
	size int64
}

func openRotatingLog(path string, max int64) (*rotatingLog, error) {
	if max <= 0 {
		return nil, errors.New("invalid log size limit")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("open debug log: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("stat debug log: %w", err)
	}
	pathInfo, err := os.Lstat(path)
	if err != nil || !pathInfo.Mode().IsRegular() || !os.SameFile(info, pathInfo) {
		_ = f.Close()
		return nil, errors.New("debug log path must be a regular file, not a symlink")
	}
	if err := f.Chmod(0600); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("protect debug log: %w", err)
	}
	return &rotatingLog{path: path, max: max, file: f, size: info.Size()}, nil
}

func (w *rotatingLog) rotate() error {
	if err := w.file.Close(); err != nil {
		return err
	}
	_ = os.Remove(w.path + ".2")
	if err := os.Rename(w.path+".1", w.path+".2"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(w.path, w.path+".1"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("reopen debug log after rotation: %w", err)
	}
	if err := f.Chmod(0600); err != nil {
		_ = f.Close()
		return err
	}
	w.file, w.size = f, 0
	return nil
}

func (w *rotatingLog) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return 0, os.ErrClosed
	}
	if w.size > 0 && w.size+int64(len(p)) > w.max {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *rotatingLog) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

var _ io.WriteCloser = (*rotatingLog)(nil)
