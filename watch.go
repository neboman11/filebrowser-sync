package main

import (
	"context"
	"io/fs"
	"log/slog"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watcher turns local filesystem activity into debounced per-folder triggers.
type Watcher struct {
	cfg     *Config
	log     *slog.Logger
	fsw     *fsnotify.Watcher
	trigger func(name string)

	mu     sync.Mutex
	timers map[string]*time.Timer
	rooted map[string]bool // folder name -> root currently watched
}

func NewWatcher(cfg *Config, log *slog.Logger, trigger func(string)) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return &Watcher{cfg: cfg, log: log, fsw: fsw, trigger: trigger,
		timers: map[string]*time.Timer{}, rooted: map[string]bool{}}, nil
}

// EnsureWatched adds watches for any folder roots not yet watched (e.g. the
// directory did not exist at startup or was remounted).
func (w *Watcher) EnsureWatched() {
	for i := range w.cfg.Folders {
		f := &w.cfg.Folders[i]
		w.mu.Lock()
		done := w.rooted[f.Name]
		w.mu.Unlock()
		if done {
			continue
		}
		if err := w.addTree(f.Local); err != nil {
			w.log.Warn("not watching folder; changes picked up by periodic sync only", "folder", f.Name, "err", err)
			continue
		}
		w.mu.Lock()
		w.rooted[f.Name] = true
		w.mu.Unlock()
	}
}

func (w *Watcher) addTree(root string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root {
				return err
			}
			return nil // vanished or unreadable subtree; skip it
		}
		if !d.IsDir() {
			return nil
		}
		if p != root && w.excluded(d.Name()) {
			return filepath.SkipDir
		}
		if err := w.fsw.Add(p); err != nil {
			if p == root {
				return err
			}
			w.log.Warn("cannot watch directory", "path", p, "err", err)
		}
		return nil
	})
}

func (w *Watcher) excluded(base string) bool {
	for _, pat := range w.cfg.Exclude {
		if strings.Contains(pat, "/") {
			continue // path patterns are left to rclone
		}
		if ok, _ := path.Match(pat, base); ok {
			return true
		}
	}
	return false
}

func (w *Watcher) folderFor(p string) *Folder {
	for i := range w.cfg.Folders {
		f := &w.cfg.Folders[i]
		if p == f.Local || strings.HasPrefix(p, f.Local+string(filepath.Separator)) {
			return f
		}
	}
	return nil
}

func (w *Watcher) Run(ctx context.Context) {
	defer w.fsw.Close()
	for {
		select {
		case <-ctx.Done():
			return
		case err := <-w.fsw.Errors:
			// Usually an inotify queue overflow: we lost events, so sync everything.
			w.log.Warn("file watcher error; scheduling full sync", "err", err)
			for _, f := range w.cfg.Folders {
				w.trigger(f.Name)
			}
		case ev := <-w.fsw.Events:
			f := w.folderFor(ev.Name)
			if f == nil || w.excluded(filepath.Base(ev.Name)) {
				continue
			}
			if ev.Name == f.Local && ev.Has(fsnotify.Remove|fsnotify.Rename) {
				w.mu.Lock()
				w.rooted[f.Name] = false
				w.mu.Unlock()
			}
			if ev.Has(fsnotify.Create) {
				// New directories (created or moved in) need their own watches.
				_ = w.addTree(ev.Name)
			}
			w.debounce(f.Name)
		}
	}
}

func (w *Watcher) debounce(name string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if t, ok := w.timers[name]; ok {
		t.Reset(w.cfg.Debounce)
		return
	}
	w.timers[name] = time.AfterFunc(w.cfg.Debounce, func() { w.trigger(name) })
}
