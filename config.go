package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Mode string

const (
	ModeTwoWay Mode = "two-way" // rclone bisync: changes and deletions flow both ways
	ModeUpload Mode = "upload"  // rclone copy local -> remote: never deletes or downloads
)

type Folder struct {
	Name   string `yaml:"name"`
	Local  string `yaml:"local"`
	Remote string `yaml:"remote"`
	Mode   Mode   `yaml:"mode"`
}

type Config struct {
	Server    string        `yaml:"server"`
	Source    string        `yaml:"source"`
	TokenFile string        `yaml:"token_file"`
	Rclone    string        `yaml:"rclone"`
	StateDir  string        `yaml:"state_dir"`
	Interval  time.Duration `yaml:"interval"`
	Debounce  time.Duration `yaml:"debounce"`
	Exclude   []string      `yaml:"exclude"`
	Folders   []Folder      `yaml:"folders"`

	token string
}

var defaultExcludes = []string{
	".~lock.*#", // LibreOffice lock files
	"~$*",       // MS Office lock files
	"*.tmp",
	"*.part",
	"*.crdownload",
	".DS_Store",
	"Thumbs.db",
	"desktop.ini",
}

var folderNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func defaultConfigPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "fbsync.yaml"
	}
	return filepath.Join(dir, "fbsync", "config.yaml")
}

func defaultStateDir() string {
	if runtime.GOOS == "linux" {
		if d := os.Getenv("XDG_STATE_HOME"); d != "" {
			return filepath.Join(d, "fbsync")
		}
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, ".local", "state", "fbsync")
		}
	}
	dir, _ := os.UserConfigDir()
	return filepath.Join(dir, "fbsync", "state")
}

func expandHome(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") && !strings.HasPrefix(p, `~\`) {
		return p, nil
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, p[1:]), nil
}

func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := &Config{
		Source:   "data",
		Rclone:   "rclone",
		Interval: 5 * time.Minute,
		Debounce: 5 * time.Second,
		Exclude:  defaultExcludes,
	}
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

func (c *Config) validate() error {
	u, err := url.Parse(c.Server)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("server must be an http(s) URL, got %q", c.Server)
	}
	c.Server = strings.TrimRight(c.Server, "/")
	if c.Source == "" || strings.Contains(c.Source, "/") {
		return fmt.Errorf("source must be a FileBrowser source name, got %q", c.Source)
	}
	if c.Interval < 10*time.Second {
		return errors.New("interval must be at least 10s")
	}
	if c.Debounce < 0 {
		return errors.New("debounce must not be negative")
	}

	if c.token = strings.TrimSpace(os.Getenv("FBSYNC_TOKEN")); c.token == "" {
		if c.TokenFile == "" {
			return errors.New("set token_file or the FBSYNC_TOKEN environment variable")
		}
		p, err := expandHome(c.TokenFile)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("token_file: %w", err)
		}
		c.token = strings.TrimSpace(string(b))
	}
	if c.token == "" {
		return errors.New("API token is empty")
	}

	if c.StateDir == "" {
		c.StateDir = defaultStateDir()
	} else if c.StateDir, err = expandHome(c.StateDir); err != nil {
		return err
	}

	if len(c.Folders) == 0 {
		return errors.New("no folders configured")
	}
	seen := map[string]bool{}
	for i := range c.Folders {
		f := &c.Folders[i]
		if !folderNameRe.MatchString(f.Name) {
			return fmt.Errorf("folder %d: name must match %s, got %q", i+1, folderNameRe, f.Name)
		}
		if seen[f.Name] {
			return fmt.Errorf("folder %q: duplicate name", f.Name)
		}
		seen[f.Name] = true
		switch f.Mode {
		case "":
			f.Mode = ModeTwoWay
		case ModeTwoWay, ModeUpload:
		default:
			return fmt.Errorf("folder %q: mode must be %q or %q", f.Name, ModeTwoWay, ModeUpload)
		}
		if f.Local, err = expandHome(f.Local); err != nil {
			return err
		}
		if !filepath.IsAbs(f.Local) {
			return fmt.Errorf("folder %q: local must be an absolute path (or start with ~)", f.Name)
		}
		f.Local = filepath.Clean(f.Local)
		f.Remote = strings.Trim(f.Remote, "/")
		if strings.Contains("/"+f.Remote+"/", "/../") {
			return fmt.Errorf("folder %q: remote must not contain ..", f.Name)
		}
	}
	for i, a := range c.Folders {
		for _, b := range c.Folders[i+1:] {
			if nested(a.Local, b.Local) {
				return fmt.Errorf("folders %q and %q: local paths overlap", a.Name, b.Name)
			}
			if nested(a.Remote, b.Remote) {
				return fmt.Errorf("folders %q and %q: remote paths overlap", a.Name, b.Name)
			}
		}
	}
	return nil
}

// nested reports whether one slash-or-separator path contains the other.
func nested(a, b string) bool {
	a, b = filepath.ToSlash(a)+"/", filepath.ToSlash(b)+"/"
	if a == "/" || b == "/" { // empty remote = whole source
		return true
	}
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

func (c *Config) Folder(name string) (*Folder, bool) {
	for i := range c.Folders {
		if c.Folders[i].Name == name {
			return &c.Folders[i], true
		}
	}
	return nil, false
}

func (c *Config) DavURL() string {
	return c.Server + "/dav/" + url.PathEscape(c.Source) + "/"
}
