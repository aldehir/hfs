package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type SSH struct {
	Host         string `yaml:"host"`
	IdentityFile string `yaml:"identity_file"`
}

type Config struct {
	Space       string `yaml:"space"`
	Profile     string `yaml:"profile"`
	AnsibleDir  string `yaml:"ansible_dir"`
	ProfilesDir string `yaml:"profiles_dir"`
	ResultsDir  string `yaml:"results_dir"`
	HfsdBinary  string `yaml:"hfsd_binary"`
	SSH         SSH    `yaml:"ssh"`

	dir string
}

// Load reads the config from path, or the first of $HFS_CONFIG, ./hfs.yml and
// ~/.config/hfs/hfs.yml when path is empty. Relative paths in the config are
// resolved against the config file's directory.
func Load(path string) (*Config, error) {
	if path == "" {
		path = find()
	}
	if path == "" {
		return nil, errors.New("no config found: pass --config, set $HFS_CONFIG, or create ./hfs.yml or ~/.config/hfs/hfs.yml")
	}
	// Follow symlinks so ~/.config/hfs/hfs.yml can point into the repo.
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	c := &Config{
		AnsibleDir:  "ansible",
		ProfilesDir: "profiles",
		ResultsDir:  "results",
		HfsdBinary:  "bin/hfsd",
		SSH:         SSH{Host: "ssh.hf.space"},
	}
	if err := yaml.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.Space == "" {
		return nil, fmt.Errorf("%s: space is required", path)
	}

	c.dir, _ = filepath.Abs(filepath.Dir(path))
	c.AnsibleDir = c.resolve(c.AnsibleDir)
	c.ProfilesDir = c.resolve(c.ProfilesDir)
	c.ResultsDir = c.resolve(c.ResultsDir)
	c.HfsdBinary = c.resolve(c.HfsdBinary)
	if c.SSH.IdentityFile != "" {
		c.SSH.IdentityFile = c.resolve(c.SSH.IdentityFile)
	}
	return c, nil
}

func find() string {
	candidates := []string{os.Getenv("HFS_CONFIG"), "hfs.yml"}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".config", "hfs", "hfs.yml"))
	}
	for _, p := range candidates {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func (c *Config) resolve(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[2:])
		}
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.dir, p)
}

// ProfilePath returns the vars file for the named profile, falling back to
// the configured default when name is empty.
func (c *Config) ProfilePath(name string) (string, error) {
	if name == "" {
		name = c.Profile
	}
	if name == "" {
		return "", errors.New("no profile given and no default profile in config")
	}
	p := filepath.Join(c.ProfilesDir, name+".yml")
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("profile %q: %w", name, err)
	}
	return p, nil
}

func (c *Config) Profiles() ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(c.ProfilesDir, "*.yml"))
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(matches))
	for _, m := range matches {
		names = append(names, strings.TrimSuffix(filepath.Base(m), ".yml"))
	}
	return names, nil
}
