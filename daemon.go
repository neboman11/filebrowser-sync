package main

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// FolderState is the observable status of one folder pair.
type FolderState struct {
	Syncing bool
	LastOK  time.Time // end of the last successful pass
	Err     error     // error from the last pass, nil if it succeeded
}

// Daemon syncs every folder on start, on local changes (debounced) and every
// cfg.Interval to pick up remote changes. Syncs run one at a time.
type Daemon struct {
	cfg *Config
	rc  *Rclone
	log *slog.Logger

	// OnChange is called after any status change. It must not block.
	OnChange func()
	// OnResult is called after every pass with its outcome.
	OnResult func(f *Folder, res SyncResult, err error)

	mu      sync.Mutex
	pending map[string]SyncOptions
	paused  bool
	state   map[string]*FolderState
	wake    chan struct{}
}

func NewDaemon(cfg *Config, rc *Rclone, log *slog.Logger) *Daemon {
	d := &Daemon{cfg: cfg, rc: rc, log: log,
		pending: map[string]SyncOptions{},
		state:   map[string]*FolderState{},
		wake:    make(chan struct{}, 1),
	}
	for _, f := range cfg.Folders {
		d.state[f.Name] = &FolderState{}
	}
	return d
}

// Request queues a pass for a folder. Options are merged with any pass
// already queued for it.
func (d *Daemon) Request(name string, opt SyncOptions) {
	d.mu.Lock()
	cur, ok := d.pending[name]
	if ok {
		opt.Resync = opt.Resync || cur.Resync
		opt.Force = opt.Force || cur.Force
	}
	d.pending[name] = opt
	d.mu.Unlock()
	d.poke()
}

func (d *Daemon) Trigger(name string) { d.Request(name, SyncOptions{}) }

func (d *Daemon) TriggerAll() {
	for _, f := range d.cfg.Folders {
		d.Trigger(f.Name)
	}
}

// SetPaused stops starting new passes (a running pass finishes). Requests
// made while paused run on resume.
func (d *Daemon) SetPaused(p bool) {
	d.mu.Lock()
	d.paused = p
	d.mu.Unlock()
	if p {
		d.log.Info("syncing paused")
	} else {
		d.log.Info("syncing resumed")
		d.poke()
	}
	d.changed()
}

func (d *Daemon) Paused() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.paused
}

// Status returns a copy of every folder's state.
func (d *Daemon) Status() map[string]FolderState {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]FolderState, len(d.state))
	for k, v := range d.state {
		out[k] = *v
	}
	return out
}

func (d *Daemon) poke() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

func (d *Daemon) changed() {
	if d.OnChange != nil {
		d.OnChange()
	}
}

// next pops the next due folder in config order, or returns nil.
func (d *Daemon) next() (*Folder, SyncOptions) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.paused {
		return nil, SyncOptions{}
	}
	for i := range d.cfg.Folders {
		f := &d.cfg.Folders[i]
		if opt, ok := d.pending[f.Name]; ok {
			delete(d.pending, f.Name)
			d.state[f.Name].Syncing = true
			return f, opt
		}
	}
	return nil, SyncOptions{}
}

func (d *Daemon) Run(ctx context.Context) error {
	w, err := NewWatcher(d.cfg, d.log, d.Trigger)
	if err != nil {
		return err
	}
	w.EnsureWatched()
	go w.Run(ctx)

	go func() {
		t := time.NewTicker(d.cfg.Interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				w.EnsureWatched()
				d.TriggerAll()
			}
		}
	}()

	d.log.Info("fbsync started", "server", d.cfg.DavURL(), "folders", len(d.cfg.Folders), "interval", d.cfg.Interval)
	d.TriggerAll()
	for {
		select {
		case <-ctx.Done():
			d.log.Info("fbsync stopping")
			return nil
		case <-d.wake:
		}
		for {
			f, opt := d.next()
			if f == nil {
				break
			}
			d.changed()
			start := time.Now()
			res, err := d.rc.Sync(ctx, f, opt)
			if ctx.Err() != nil {
				d.log.Info("fbsync stopping")
				return nil
			}

			d.mu.Lock()
			st := d.state[f.Name]
			st.Syncing, st.Err = false, err
			if err == nil {
				st.LastOK = time.Now()
			}
			d.mu.Unlock()

			if err != nil {
				d.log.Error("sync failed; will retry", "folder", f.Name, "err", err)
			} else {
				level := slog.LevelDebug
				if res.Changed {
					level = slog.LevelInfo
				}
				d.log.Log(ctx, level, "sync complete", "folder", f.Name, "took", time.Since(start).Round(time.Millisecond))
			}
			if d.OnResult != nil {
				d.OnResult(f, res, err)
			}
			d.changed()
		}
	}
}
