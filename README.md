# fbsync

OneDrive-style folder sync for [FileBrowser Quantum](https://github.com/gtsteffaniak/filebrowser).
It lives in the system tray, watches chosen local folders and keeps them in
sync with folders on your FileBrowser server, using the server's built-in
WebDAV endpoint and [rclone](https://rclone.org) for the transfers.

- **two-way** folders: edits, new files and deletions flow both ways
  (`rclone bisync`). Conflicting edits: newer copy wins, the other is kept as
  `<name>.conflict1`.
- **upload** folders: local new/changed files are pushed; the server copy is
  never deleted and nothing is downloaded (`rclone copy`). Good for camera
  rolls, backups, exports.
- Local changes sync within seconds (file watcher + debounce); server-side
  changes (web UI, other machines) are picked up every `interval`.
- Modification times are preserved, so same-size edits are detected.

No server changes are needed: WebDAV is on by default in Quantum.

## Install

Needs rclone >= 1.66 and Go to build.

```sh
sudo pacman -S rclone          # or your distro's package / https://rclone.org/downloads
make install                   # ~/.local/bin/fbsync, app-menu entry, autostart at login,
                               # ~/.config/fbsync/config.yaml from the example
```

## Setup

1. In FileBrowser open **Settings -> API Tokens** and create a token with
   customization **off** (WebDAV needs the minimal token). Pick a long expiry.
   Your user needs View, Download, Create, Modify and Delete on the source.
2. Save it: `install -m600 /dev/stdin ~/.config/fbsync/token` then paste, Ctrl-D.
3. Start **fbsync** from the application menu (it also starts at every login).
   Use **Edit config** in the tray menu to choose folders, then **Reload config**.

## The tray icon

| Icon | Meaning |
| --- | --- |
| green | everything up to date |
| blue, spinning | syncing |
| grey | paused |
| red | a folder has a problem, or the config is invalid (details in the menu) |

The menu shows the overall state and has **Sync now**, **Pause syncing**, and
one submenu per folder with its status, **Open folder**, **Open in
FileBrowser** and **Sync now**. Below that: **Open FileBrowser**, **Open log**,
**Edit config**, **Reload config** and **Quit**.

Desktop notifications appear for conflicting edits and when a folder starts
failing (once, not on every retry).

## How the first sync works

The first sync of a folder pair (and any sync after changing its `local`,
`remote`, `mode` or `exclude`) is a *baseline*: both sides are merged, the
newer copy of a differing file wins, and **nothing is deleted**. After that,
deletions propagate normally for two-way folders.

## Safety

- If an established local folder disappears (unmounted disk, renamed), fbsync
  refuses to sync it instead of treating it as "everything was deleted".
- bisync refuses any pass that would delete more than half of a folder's
  entries on one side, and nothing is changed. The folder turns red and its
  submenu offers **Confirm deletions (delete on both sides)** if it was
  intended, or **Restore deleted files** to bring them back from the other side.
- Only one fbsync (tray, service or `once`) runs at a time.
- Sync state lives in `~/.local/state/fbsync`; deleting it just causes a
  baseline on next run. The tray log is `~/.local/state/fbsync/fbsync.log`.

## Command line / headless

```
fbsync [-config FILE] [-v] tray                               tray icon (what the menu entry runs)
fbsync [-config FILE] [-v] run                                no UI, log to stderr
fbsync [-config FILE] [-v] once [-resync|-force] [NAME...]    sync once and exit
fbsync [-config FILE] check                                   test token/server, show folder state
```

- `once -resync NAME`: merge both sides, newer wins, nothing deleted (same as
  **Restore deleted files**).
- `once -force NAME`: apply a blocked mass deletion (same as **Confirm deletions**).

For a machine without a desktop, use the systemd user service instead of the
tray (not both):

```sh
make install-service
systemctl --user enable --now fbsync
journalctl --user -u fbsync -f
loginctl enable-linger $USER       # keep syncing while logged out
```

## Notes / limitations

- Server-side changes are polled, not pushed: worst-case delay is `interval`.
  Each poll lists the whole folder pair, so keep huge trees on a longer interval.
- Files are uploaded in a single request; the HTTPRoute already has a 10h
  timeout and no body-size limit.
- `.conflictN` copies are left for you to merge and delete.
- Tray: Linux (any StatusNotifierItem host: KDE Plasma, GNOME with the
  AppIndicator extension, waybar, ...) and Windows. macOS builds need cgo and
  must be built on a Mac. Desktop notifications are Linux only.
