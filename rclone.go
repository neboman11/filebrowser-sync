package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// remoteName is the rclone remote defined purely through environment variables,
// so fbsync never touches the user's rclone.conf.
const remoteName = "fbsync"

// rclone >= 1.66 is required for bisync --resync-mode/--conflict-resolve/--recover.
const minMajor, minMinor = 1, 66

type Rclone struct {
	cfg *Config
	env []string
	log *slog.Logger
}

func NewRclone(ctx context.Context, cfg *Config, log *slog.Logger) (*Rclone, error) {
	out, err := exec.CommandContext(ctx, cfg.Rclone, "version").Output()
	if err != nil {
		return nil, fmt.Errorf("running %q: %w (install rclone >= %d.%d)", cfg.Rclone, err, minMajor, minMinor)
	}
	m := regexp.MustCompile(`rclone v(\d+)\.(\d+)`).FindSubmatch(out)
	if m == nil {
		return nil, fmt.Errorf("cannot parse rclone version from %q", firstLine(out))
	}
	major, _ := strconv.Atoi(string(m[1]))
	minor, _ := strconv.Atoi(string(m[2]))
	if major < minMajor || (major == minMajor && minor < minMinor) {
		return nil, fmt.Errorf("rclone v%d.%d is too old, need >= %d.%d", major, minor, minMajor, minMinor)
	}

	// The WebDAV password must be in rclone's obscured form. Pipe the token over
	// stdin so it never shows up in the process list.
	cmd := exec.CommandContext(ctx, cfg.Rclone, "obscure", "-")
	cmd.Stdin = strings.NewReader(cfg.token)
	obscured, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("rclone obscure: %w", err)
	}

	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, err
	}
	// Point rclone at a private (empty) config so a user's rclone.conf cannot
	// shadow or alter the fbsync remote, and so rclone stops warning about it.
	rcConf := filepath.Join(cfg.StateDir, "rclone.conf")
	if f, err := os.OpenFile(rcConf, os.O_CREATE|os.O_RDONLY, 0o600); err == nil {
		f.Close()
	}

	prefix := "RCLONE_CONFIG_" + strings.ToUpper(remoteName) + "_"
	env := append(os.Environ(),
		"RCLONE_CONFIG="+rcConf,
		prefix+"TYPE=webdav",
		prefix+"URL="+cfg.DavURL(),
		// FileBrowser Quantum honours X-OC-Mtime on PUT but rejects PROPPATCH and
		// serves no hashes. vendor=rclone is the only vendor mode that sends
		// X-OC-Mtime without assuming either of those, which preserves
		// modification times (needed to detect same-size edits).
		prefix+"VENDOR=rclone",
		prefix+"USER=fbsync", // ignored by FileBrowser
		prefix+"PASS="+strings.TrimSpace(string(obscured)),
	)
	return &Rclone{cfg: cfg, env: env, log: log}, nil
}

func firstLine(b []byte) string {
	s, _, _ := strings.Cut(string(b), "\n")
	return s
}

func remotePath(f *Folder) string { return remoteName + ":" + f.Remote }

// errNeedsResync is returned when bisync aborted and its listings are unusable.
var errNeedsResync = errors.New("bisync requires --resync")

// errTooManyDeletes is bisync's safety abort when a pass would delete more
// than half the files on one side. It repeats until a human decides.
var errTooManyDeletes = errors.New("too many deletions")

// commonArgs: INFO-level output without timestamps or colour, so run() can
// classify each line. Progress stats are emitted every minute during long
// transfers.
var commonArgs = []string{"-v", "--color", "NEVER", "--log-format", ",",
	"--stats", "1m", "--stats-one-line", "--stats-log-level", "INFO"}

// changeRe matches rclone's per-file INFO lines that describe a real change.
var changeRe = regexp.MustCompile(`: (Copied|Deleted|Moved|Updated|Made directory|Removing directory|Removed directory)`)

// bisync flags a path changed on both sides with conflictMarker (path after
// the last " - "). It is only a real conflict if the contents differ, in which
// case bisync follows up by renaming the losing copy (conflictRename).
const (
	conflictMarker = "New or changed in both paths"
	conflictRename = "Renaming Path"
)

// SyncResult describes what a pass did.
type SyncResult struct {
	Changed   bool     // at least one file or directory was transferred/deleted
	Conflicts []string // paths edited on both sides (loser kept as .conflictN)
}

// run executes rclone and routes its output into our logger: errors and notices
// (including bisync conflict renames) as warnings/errors, file changes as info,
// everything else as debug.
func (r *Rclone) run(ctx context.Context, f *Folder, args ...string) (SyncResult, error) {
	cmd := exec.CommandContext(ctx, r.cfg.Rclone, append(args, commonArgs...)...)
	cmd.Env = r.env
	// Let rclone (bisync in particular) shut down cleanly and release its lock.
	cmd.Cancel = func() error { return interrupt(cmd.Process) }
	cmd.WaitDelay = 30 * time.Second

	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	type result struct {
		SyncResult
		needsResync, tooManyDeletes bool
	}
	done := make(chan result, 1)
	go func() {
		var res result
		var candidate string // last path bisync saw changed on both sides
		level := slog.LevelDebug
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			line := strings.TrimRight(sc.Text(), " ")
			if line == "" {
				continue
			}
			// Lines without a level prefix continue the previous message.
			switch {
			case strings.HasPrefix(line, "ERROR"), strings.HasPrefix(line, "CRITICAL"):
				level = slog.LevelError
			case strings.HasPrefix(line, "NOTICE"):
				level = slog.LevelWarn
				if strings.Contains(line, " / ") && strings.Contains(line, "ETA") {
					level = slog.LevelDebug // final stats summary
				}
			case strings.HasPrefix(line, "INFO"):
				level = slog.LevelDebug
				if changeRe.MatchString(line) {
					level, res.Changed = slog.LevelInfo, true
				} else if strings.Contains(line, "ETA") && !strings.Contains(line, " 0 B / 0 B") {
					level = slog.LevelInfo // periodic transfer progress
				}
			case strings.HasPrefix(line, "DEBUG"):
				level = slog.LevelDebug
			}
			switch {
			case strings.Contains(line, "Must run --resync"):
				res.needsResync = true
			case strings.Contains(line, "Safety abort: too many deletes"):
				res.tooManyDeletes = true
			case strings.Contains(line, conflictMarker):
				if i := strings.LastIndex(line, " - "); i >= 0 {
					candidate = strings.TrimSpace(line[i+3:])
				}
			case strings.Contains(line, conflictRename) && candidate != "":
				res.Conflicts = append(res.Conflicts, candidate)
				candidate = ""
			}
			if _, msg, ok := strings.Cut(line, ": "); ok && level != slog.LevelDebug {
				line = msg
			}
			r.log.Log(ctx, level, line, "folder", f.Name)
		}
		_, _ = io.Copy(io.Discard, pr) // keep draining if a line exceeded the buffer
		done <- res
	}()

	err := cmd.Run()
	pw.Close()
	res := <-done
	switch {
	case res.needsResync:
		return res.SyncResult, errNeedsResync
	case res.tooManyDeletes:
		return res.SyncResult, errTooManyDeletes
	case err != nil && ctx.Err() != nil:
		return res.SyncResult, ctx.Err()
	}
	return res.SyncResult, err
}

// Check verifies credentials and that the configured remote folders are reachable.
func (r *Rclone) Check(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, r.cfg.Rclone, "lsf", "--max-depth", "1", "--dirs-only", remoteName+":")
	cmd.Env = r.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("listing %s failed: %w\n%s", r.cfg.DavURL(), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (r *Rclone) filtersFile(f *Folder) (string, error) {
	var b strings.Builder
	b.WriteString("# generated by fbsync; edit `exclude` in the config instead\n")
	for _, p := range r.cfg.Exclude {
		b.WriteString("- " + p + "\n")
	}
	p := filepath.Join(r.cfg.StateDir, f.Name+".filters")
	return p, writeIfChanged(p, b.String())
}

func writeIfChanged(path, content string) error {
	if old, err := os.ReadFile(path); err == nil && string(old) == content {
		return nil
	}
	return os.WriteFile(path, []byte(content), 0o600)
}

// markerPath records that a folder pair completed a baseline sync with a given
// identity (mode, local path, remote URL/path, filters). Any change forces a
// new baseline.
func (r *Rclone) markerPath(f *Folder) string {
	return filepath.Join(r.cfg.StateDir, f.Name+".synced")
}

func (r *Rclone) identity(f *Folder) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%s", f.Mode, f.Local, r.cfg.DavURL(), f.Remote, strings.Join(r.cfg.Exclude, "\x00"))
	return hex.EncodeToString(h.Sum(nil))
}

func (r *Rclone) needsBaseline(f *Folder) bool {
	b, err := os.ReadFile(r.markerPath(f))
	return err != nil || strings.TrimSpace(string(b)) != r.identity(f)
}

func (r *Rclone) forgetBaseline(f *Folder) error {
	err := os.Remove(r.markerPath(f))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// SyncOptions alter a single pass.
type SyncOptions struct {
	// Resync re-establishes the baseline: merge both sides, newer wins,
	// nothing deleted.
	Resync bool
	// Force applies mass deletions that bisync would otherwise refuse.
	Force bool
}

// Sync runs one synchronisation pass for f.
func (r *Rclone) Sync(ctx context.Context, f *Folder, opt SyncOptions) (SyncResult, error) {
	if opt.Resync {
		if err := r.forgetBaseline(f); err != nil {
			return SyncResult{}, err
		}
	}
	filters, err := r.filtersFile(f)
	if err != nil {
		return SyncResult{}, err
	}
	baseline := r.needsBaseline(f)

	if baseline {
		r.log.Info("establishing baseline", "folder", f.Name, "mode", f.Mode, "local", f.Local, "remote", "/"+f.Remote)
		// Only create the roots when establishing a pair. If an established
		// local folder vanishes (e.g. unmounted disk) bisync must fail rather
		// than see an empty tree and propagate mass deletions.
		if err := os.MkdirAll(f.Local, 0o755); err != nil {
			return SyncResult{}, err
		}
		if _, err := r.run(ctx, f, "mkdir", remotePath(f)); err != nil {
			return SyncResult{}, fmt.Errorf("creating remote folder: %w", err)
		}
	}

	var args []string
	switch f.Mode {
	case ModeUpload:
		args = []string{"copy", f.Local, remotePath(f), "--create-empty-src-dirs"}
	case ModeTwoWay:
		args = []string{
			"bisync", f.Local, remotePath(f),
			"--workdir", filepath.Join(r.cfg.StateDir, "bisync"),
			"--compare", "size,modtime",
			"--conflict-resolve", "newer",
			"--conflict-loser", "num",
			"--create-empty-src-dirs",
			"--resilient", "--recover",
			"--max-lock", "15m",
		}
		if baseline {
			// Union of both sides; where a file differs, the newer copy wins.
			// Nothing is deleted during a resync.
			args = append(args, "--resync", "--resync-mode", "newer")
		}
		if opt.Force {
			args = append(args, "--force")
		}
	}
	args = append(args, "--filter-from", filters)

	res, err := r.run(ctx, f, args...)
	switch {
	case errors.Is(err, errNeedsResync):
		if ferr := r.forgetBaseline(f); ferr != nil {
			return res, ferr
		}
		return res, fmt.Errorf("%w; next pass will re-establish the baseline (union, newer wins, no deletions)", err)
	case errors.Is(err, errTooManyDeletes):
		return res, fmt.Errorf("%w; nothing was changed. If intended run `fbsync once -force %s`, "+
			"to restore the files run `fbsync once -resync %s`", err, f.Name, f.Name)
	case err != nil:
		return res, err
	case baseline:
		return res, os.WriteFile(r.markerPath(f), []byte(r.identity(f)+"\n"), 0o600)
	}
	return res, nil
}
