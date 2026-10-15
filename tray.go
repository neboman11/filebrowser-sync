package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"fyne.io/systray"
)

//go:embed config.example.yaml
var exampleConfig []byte

var errAlreadyRunning = errors.New("fbsync is already running (tray or service)")

// instanceLockPath is per user, independent of the config, so a tray and a
// service (or `once`) can never sync the same folders concurrently.
func instanceLockPath() string { return filepath.Join(defaultStateDir(), "instance.lock") }

func logFilePath() string { return filepath.Join(defaultStateDir(), "fbsync.log") }

type folderMenu struct {
	root, status, force, resync *systray.MenuItem
}

// Tray runs the sync daemon in-process behind a system tray icon.
type Tray struct {
	cfgPath string
	log     *slog.Logger
	icons   *icons

	reloadMu sync.Mutex // serialises reload/quit

	mu          sync.Mutex
	cfg         *Config
	cfgErr      error
	daemon      *Daemon
	stopDaemon  context.CancelFunc
	daemonDone  chan struct{}
	menuStop    context.CancelFunc
	status      *systray.MenuItem
	syncNow     *systray.MenuItem
	pause       *systray.MenuItem
	folders     map[string]*folderMenu
	errNotified map[string]bool
	lastIcon    []byte
	quitting    bool

	update chan struct{}
}

func runTray(ctx context.Context, cfgPath string, log *slog.Logger) error {
	if err := os.MkdirAll(defaultStateDir(), 0o700); err != nil {
		return err
	}
	release, err := lockInstance(instanceLockPath())
	if errors.Is(err, errAlreadyRunning) {
		notify("fbsync is already running", "Look for its icon in the system tray.")
	}
	if err != nil {
		return err
	}
	defer release()

	t := &Tray{cfgPath: cfgPath, log: log, icons: renderIcons(),
		errNotified: map[string]bool{}, update: make(chan struct{}, 1)}
	go func() {
		<-ctx.Done()
		t.quit()
	}()
	systray.Run(t.onReady, nil)
	return nil
}

func (t *Tray) onReady() {
	systray.SetTitle("fbsync")
	systray.SetIcon(t.icons.idle)
	systray.SetTooltip("fbsync")
	go t.renderLoop()
	t.reload()
}

func (t *Tray) requestRender() {
	select {
	case t.update <- struct{}{}:
	default:
	}
}

// reload (re)starts the daemon from the config file and rebuilds the menu.
func (t *Tray) reload() {
	t.reloadMu.Lock()
	defer t.reloadMu.Unlock()
	t.stop()

	cfg, err := LoadConfig(t.cfgPath)
	var rc *Rclone
	if err == nil {
		rc, err = NewRclone(context.Background(), cfg, t.log)
	}
	t.mu.Lock()
	if t.quitting {
		t.mu.Unlock()
		return
	}
	t.cfg, t.cfgErr, t.daemon = cfg, err, nil
	if err != nil {
		t.log.Error("cannot start syncing", "err", err)
		notify("fbsync: configuration problem", err.Error())
	} else {
		d := NewDaemon(cfg, rc, t.log)
		d.OnChange = t.requestRender
		d.OnResult = t.onResult
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		t.daemon, t.stopDaemon, t.daemonDone = d, cancel, done
		go func() {
			defer close(done)
			if err := d.Run(ctx); err != nil {
				t.log.Error("sync engine stopped", "err", err)
				t.mu.Lock()
				t.cfgErr, t.daemon = err, nil
				t.mu.Unlock()
				t.requestRender()
			}
		}()
	}
	t.mu.Unlock()
	t.buildMenu()
	t.requestRender()
}

// stop cancels the running daemon and waits for rclone to exit cleanly.
func (t *Tray) stop() {
	t.mu.Lock()
	cancel, done := t.stopDaemon, t.daemonDone
	t.stopDaemon, t.daemonDone = nil, nil
	t.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

func (t *Tray) quit() {
	t.mu.Lock()
	t.quitting = true
	if t.status != nil {
		t.status.SetTitle("Stopping…")
	}
	t.mu.Unlock()
	t.reloadMu.Lock()
	t.stop()
	t.reloadMu.Unlock()
	systray.Quit()
}

func onClick(ctx context.Context, item *systray.MenuItem, fn func()) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-item.ClickedCh:
				fn()
			}
		}
	}()
}

func (t *Tray) buildMenu() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.menuStop != nil {
		t.menuStop()
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.menuStop = cancel
	systray.ResetMenu()

	t.status = systray.AddMenuItem("Starting…", "")
	t.status.Disable()
	systray.AddSeparator()
	t.syncNow = systray.AddMenuItem("Sync now", "Sync every folder now")
	t.pause = systray.AddMenuItemCheckbox("Pause syncing", "Stop starting new syncs until resumed", false)
	onClick(ctx, t.syncNow, func() { t.withDaemon(func(d *Daemon) { d.TriggerAll() }) })
	onClick(ctx, t.pause, func() { t.withDaemon(func(d *Daemon) { d.SetPaused(!d.Paused()) }) })

	t.folders = map[string]*folderMenu{}
	if t.cfg != nil && t.daemon != nil {
		systray.AddSeparator()
		for i := range t.cfg.Folders {
			f := &t.cfg.Folders[i]
			fm := &folderMenu{root: systray.AddMenuItem(f.Name, f.Local)}
			fm.status = fm.root.AddSubMenuItem("", "")
			fm.status.Disable()
			open := fm.root.AddSubMenuItem("Open folder", f.Local)
			web := fm.root.AddSubMenuItem("Open in FileBrowser", "")
			now := fm.root.AddSubMenuItem("Sync now", "")
			fm.force = fm.root.AddSubMenuItem("Confirm deletions (delete on both sides)",
				"Apply the deletions that were blocked because they affect more than half the folder")
			fm.resync = fm.root.AddSubMenuItem("Restore deleted files",
				"Merge both sides again: nothing is deleted, newer copies win")
			fm.force.Hide()
			fm.resync.Hide()
			t.folders[f.Name] = fm

			name, local, webURL := f.Name, f.Local, t.cfg.webURL(f)
			onClick(ctx, open, func() { t.open(local) })
			onClick(ctx, web, func() { t.open(webURL) })
			onClick(ctx, now, func() { t.withDaemon(func(d *Daemon) { d.Trigger(name) }) })
			onClick(ctx, fm.force, func() {
				t.withDaemon(func(d *Daemon) { d.Request(name, SyncOptions{Force: true}) })
			})
			onClick(ctx, fm.resync, func() {
				t.withDaemon(func(d *Daemon) { d.Request(name, SyncOptions{Resync: true}) })
			})
		}
	}

	systray.AddSeparator()
	if t.cfg != nil {
		server := t.cfg.Server
		onClick(ctx, systray.AddMenuItem("Open FileBrowser", server), func() { t.open(server) })
	}
	onClick(ctx, systray.AddMenuItem("Open log", logFilePath()), func() { t.open(logFilePath()) })
	onClick(ctx, systray.AddMenuItem("Edit config", t.cfgPath), t.editConfig)
	onClick(ctx, systray.AddMenuItem("Reload config", "Apply changes made to the config file"), func() { go t.reload() })
	systray.AddSeparator()
	onClick(ctx, systray.AddMenuItem("Quit", "Stop syncing and remove the tray icon"), func() { go t.quit() })
}

func (t *Tray) withDaemon(fn func(*Daemon)) {
	t.mu.Lock()
	d := t.daemon
	t.mu.Unlock()
	if d != nil {
		fn(d)
	}
}

func (t *Tray) open(target string) {
	if err := openPath(target); err != nil {
		t.log.Error("cannot open", "target", target, "err", err)
	}
}

// editConfig opens the config file, creating it from the example first.
func (t *Tray) editConfig() {
	if _, err := os.Stat(t.cfgPath); errors.Is(err, os.ErrNotExist) {
		err = os.MkdirAll(filepath.Dir(t.cfgPath), 0o700)
		if err == nil {
			err = os.WriteFile(t.cfgPath, exampleConfig, 0o600)
		}
		if err != nil {
			t.log.Error("cannot create config", "path", t.cfgPath, "err", err)
			return
		}
	}
	t.open(t.cfgPath)
}

func (t *Tray) onResult(f *Folder, res SyncResult, err error) {
	if n := len(res.Conflicts); n > 0 {
		shown := res.Conflicts[:min(n, 5)]
		body := fmt.Sprintf("%d file(s) changed on both sides. The newer copy was kept; the other is saved alongside as .conflictN:\n%s",
			n, strings.Join(shown, "\n"))
		notify("Conflicting edits in "+f.Name, body)
	}
	t.mu.Lock()
	first := err != nil && !t.errNotified[f.Name]
	t.errNotified[f.Name] = err != nil
	t.mu.Unlock()
	if first {
		msg := t.shortErr(err)
		if errors.Is(err, errTooManyDeletes) {
			msg += ". Use the tray menu to confirm the deletions or restore the files."
		}
		notify("Sync problem: "+f.Name, msg)
	}
}

func (t *Tray) shortErr(err error) string {
	switch {
	case errors.Is(err, errTooManyDeletes):
		return "Paused: this would delete more than half of the folder"
	case errors.Is(err, errNeedsResync):
		return "Sync state was reset; it will re-merge on the next pass"
	}
	// One line, without the config path LoadConfig prefixes (it is in the menu).
	s := strings.Join(strings.Fields(err.Error()), " ")
	s = strings.TrimPrefix(s, t.cfgPath+": ")
	if r := []rune(s); len(r) > 120 {
		s = string(r[:119]) + "…"
	}
	return s
}

func (t *Tray) renderLoop() {
	tick := time.NewTicker(150 * time.Millisecond)
	defer tick.Stop()
	frame := 0
	for {
		select {
		case <-t.update:
		case <-tick.C:
			if !t.anySyncing() {
				continue
			}
			frame = (frame + 1) % spinFrames
		}
		t.render(frame)
	}
}

func (t *Tray) anySyncing() bool {
	t.mu.Lock()
	d := t.daemon
	t.mu.Unlock()
	if d == nil {
		return false
	}
	for _, s := range d.Status() {
		if s.Syncing {
			return true
		}
	}
	return false
}

func (t *Tray) render(frame int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.status == nil || t.quitting {
		return
	}

	var icon []byte
	var summary string
	switch {
	case t.cfgErr != nil:
		icon, summary = t.icons.err, "Not syncing: "+t.shortErr(t.cfgErr)
		t.syncNow.Disable()
		t.pause.Disable()
	case t.daemon != nil:
		st := t.daemon.Status()
		paused := t.daemon.Paused()
		var syncing, failed []string
		var last time.Time
		for _, f := range t.cfg.Folders {
			s := st[f.Name]
			if s.Syncing {
				syncing = append(syncing, f.Name)
			}
			if s.Err != nil {
				failed = append(failed, f.Name)
			}
			if s.LastOK.After(last) {
				last = s.LastOK
			}
			t.renderFolder(t.folders[f.Name], f.Name, s)
		}
		switch {
		case len(syncing) > 0:
			icon, summary = t.icons.spin[frame], "Syncing "+strings.Join(syncing, ", ")+"…"
		case paused:
			icon, summary = t.icons.paused, "Paused"
		case len(failed) > 0:
			icon, summary = t.icons.err, "Problem with "+strings.Join(failed, ", ")
		case last.IsZero():
			icon, summary = t.icons.idle, "Waiting for first sync"
		default:
			icon, summary = t.icons.idle, "Up to date · "+last.Format("15:04")
		}
		if paused {
			t.pause.Check()
			t.syncNow.Disable()
		} else {
			t.pause.Uncheck()
			t.syncNow.Enable()
		}
		t.pause.Enable()
	}

	t.status.SetTitle(summary)
	systray.SetTooltip("fbsync: " + summary)
	if !slices.Equal(icon, t.lastIcon) {
		systray.SetIcon(icon)
		t.lastIcon = icon
	}
}

func (t *Tray) renderFolder(fm *folderMenu, name string, s FolderState) {
	if fm == nil {
		return
	}
	title, status := name, ""
	switch {
	case s.Syncing:
		title, status = name+" (syncing)", "Syncing…"
	case s.Err != nil:
		title, status = name+" (problem)", t.shortErr(s.Err)
	case s.LastOK.IsZero():
		status = "Waiting for first sync"
	default:
		status = "Up to date · synced " + s.LastOK.Format("15:04")
	}
	fm.root.SetTitle(title)
	fm.status.SetTitle(status)
	if errors.Is(s.Err, errTooManyDeletes) && !s.Syncing {
		fm.force.Show()
		fm.resync.Show()
	} else {
		fm.force.Hide()
		fm.resync.Hide()
	}
}

// webURL is the FileBrowser web UI address of a folder's remote path.
func (c *Config) webURL(f *Folder) string {
	u := c.Server + "/files/" + url.PathEscape(c.Source) + "/"
	if f.Remote != "" {
		parts := strings.Split(f.Remote, "/")
		for i, p := range parts {
			parts[i] = url.PathEscape(p)
		}
		u += strings.Join(parts, "/") + "/"
	}
	return u
}
