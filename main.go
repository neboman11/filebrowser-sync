// Command fbsync keeps local folders in sync with a FileBrowser Quantum server
// over its WebDAV endpoint, using rclone for the transfers.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

const usage = `fbsync - sync local folders with FileBrowser Quantum

Usage:
  fbsync [-config FILE] [-v] tray                            run with a system tray icon
  fbsync [-config FILE] [-v] run                             watch folders and sync continuously
  fbsync [-config FILE] [-v] once [-resync|-force] [NAME...] sync folders once (default: all) and exit
  fbsync [-config FILE] check                                verify rclone, token and server access

-v logs every rclone message, not just changes, warnings and errors.
-resync re-establishes the baseline: both sides are merged, the newer copy of a
differing file wins, and nothing is deleted. -force applies deletions that
bisync refused because they affect more than half of a folder.

Only one of tray/run/once can be active at a time.
Default config: %s
Tray log:       %s
`

const maxLogSize = 5 << 20

func main() {
	fl := flag.NewFlagSet("fbsync", flag.ExitOnError)
	cfgPath := fl.String("config", defaultConfigPath(), "config file")
	verbose := fl.Bool("v", false, "verbose logging")
	fl.Usage = func() { fmt.Fprintf(os.Stderr, usage, defaultConfigPath(), logFilePath()) }
	_ = fl.Parse(os.Args[1:])
	if fl.NArg() == 0 {
		fl.Usage()
		os.Exit(2)
	}
	cmd := fl.Arg(0)

	var out io.Writer = os.Stderr
	if cmd == "tray" {
		// Started from the desktop there is no terminal: keep a log file.
		f, err := openLogFile(logFilePath())
		if err != nil {
			fmt.Fprintln(os.Stderr, "fbsync: log file:", err)
		} else {
			defer f.Close()
			out = io.MultiWriter(os.Stderr, f)
		}
	}
	opts := &slog.HandlerOptions{}
	if *verbose {
		opts.Level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(out, opts))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := dispatch(ctx, log, *cfgPath, cmd, fl.Args()[1:]); err != nil {
		log.Error(err.Error())
		os.Exit(1)
	}
}

// openLogFile appends to path, moving it to path.1 first once it is large.
func openLogFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if fi, err := os.Stat(path); err == nil && fi.Size() > maxLogSize {
		_ = os.Rename(path, path+".1")
	}
	return os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
}

func dispatch(ctx context.Context, log *slog.Logger, cfgPath, cmd string, args []string) error {
	switch cmd {
	case "tray":
		return runTray(ctx, cfgPath, log)
	case "run", "once", "check":
	default:
		return fmt.Errorf("unknown command %q (want tray, run, once or check)", cmd)
	}

	if cmd != "check" {
		if err := os.MkdirAll(defaultStateDir(), 0o700); err != nil {
			return err
		}
		release, err := lockInstance(instanceLockPath())
		if errors.Is(err, errAlreadyRunning) {
			return fmt.Errorf("%w; use the tray menu, or stop it first (quit the tray / systemctl --user stop fbsync)", err)
		}
		if err != nil {
			return err
		}
		defer release()
	}

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		return err
	}
	rc, err := NewRclone(ctx, cfg, log)
	if err != nil {
		return err
	}

	switch cmd {
	case "check":
		if err := rc.Check(ctx); err != nil {
			return err
		}
		fmt.Printf("ok: authenticated to %s\n", cfg.DavURL())
		for _, f := range cfg.Folders {
			state := "established"
			if rc.needsBaseline(&f) {
				state = "baseline pending (first sync merges both sides)"
			}
			fmt.Printf("  %-16s %-8s %s <-> /%s  [%s]\n", f.Name, f.Mode, f.Local, f.Remote, state)
		}
		return nil

	case "once":
		fs := flag.NewFlagSet("once", flag.ExitOnError)
		resync := fs.Bool("resync", false, "re-establish baseline (merge, newer wins, no deletions)")
		force := fs.Bool("force", false, "apply deletions of more than half a folder (two-way only)")
		_ = fs.Parse(args)
		folders, err := selectFolders(cfg, fs.Args())
		if err != nil {
			return err
		}
		var failed []string
		for _, f := range folders {
			if _, err := rc.Sync(ctx, f, SyncOptions{Resync: *resync, Force: *force}); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				log.Error("sync failed", "folder", f.Name, "err", err)
				failed = append(failed, f.Name)
				continue
			}
			log.Info("sync complete", "folder", f.Name)
		}
		if len(failed) > 0 {
			return fmt.Errorf("sync failed for: %v", failed)
		}
		return nil
	}

	return NewDaemon(cfg, rc, log).Run(ctx)
}

func selectFolders(cfg *Config, names []string) ([]*Folder, error) {
	if len(names) == 0 {
		out := make([]*Folder, len(cfg.Folders))
		for i := range cfg.Folders {
			out[i] = &cfg.Folders[i]
		}
		return out, nil
	}
	var out []*Folder
	for _, n := range names {
		f, ok := cfg.Folder(n)
		if !ok {
			return nil, fmt.Errorf("no folder named %q in config", n)
		}
		out = append(out, f)
	}
	return out, nil
}
