package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Host              string   `yaml:"host"`
	Backend           string   `yaml:"backend"`
	GuestBinary       string   `yaml:"guest_binary"`
	T3PublicURL       string   `yaml:"t3_public_url"`
	User              string   `yaml:"user"`
	Org               string   `yaml:"org"`
	Provider          string   `yaml:"provider"`
	Repo              string   `yaml:"repo"`
	Base              string   `yaml:"base"`
	AutoSuspend       string   `yaml:"auto_suspend"`
	Snapshot          string   `yaml:"snapshot"`
	VCPUs             int      `yaml:"vcpus"`
	Memory            int      `yaml:"mem_mib"`
	Secrets           []string `yaml:"secrets"`
	CredentialBackend string   `yaml:"credential_backend"`
	CredentialKeyFile string   `yaml:"credential_key_file"`
	LocalRepoRoot     string   `yaml:"local_repo_root"`
	LocalWorktreeRoot string   `yaml:"local_worktree_root"`
	LocalT3Bin        string   `yaml:"local_t3_bin"`
	LocalCodexBin     string   `yaml:"local_codex_bin"`
	LocalT3Port       int      `yaml:"local_t3_port"`
}

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,95}$`)
var repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var secretPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

func validID(s string) error {
	if !idPattern.MatchString(s) {
		return errors.New("invalid identifier")
	}
	return nil
}

func defaultConfig() Config {
	return Config{Host: "instabox", Provider: "codex", Base: "main", AutoSuspend: "5m", VCPUs: 2, Memory: 4096, LocalT3Bin: "t3", LocalCodexBin: "codex", LocalT3Port: 3774}
}

func decodeConfig(r io.Reader) (Config, error) {
	c := defaultConfig()
	d := yaml.NewDecoder(r)
	d.KnownFields(true)
	if err := d.Decode(&c); err != nil && err != io.EOF {
		return c, errors.New("invalid YAML configuration; use known non-secret fields only")
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return c, errors.New("use a single YAML document")
	}
	return c, nil
}

func loadConfig(path string, env func(string) (string, bool)) (Config, error) {
	c := defaultConfig()
	if path != "" {
		f, err := os.Open(path)
		if err != nil {
			return c, errors.New("cannot open configuration file")
		}
		defer f.Close()
		v, err := decodeConfig(f)
		if err != nil {
			return c, err
		}
		c = v
	}
	for k, p := range map[string]*string{"HOST": &c.Host, "BACKEND": &c.Backend, "GUEST_BINARY": &c.GuestBinary, "T3_PUBLIC_URL": &c.T3PublicURL, "USER": &c.User, "ORG": &c.Org, "PROVIDER": &c.Provider, "REPO": &c.Repo, "BASE": &c.Base, "AUTO_SUSPEND": &c.AutoSuspend, "SNAPSHOT": &c.Snapshot, "CREDENTIAL_BACKEND": &c.CredentialBackend, "CREDENTIAL_KEY_FILE": &c.CredentialKeyFile, "LOCAL_REPO_ROOT": &c.LocalRepoRoot, "LOCAL_WORKTREE_ROOT": &c.LocalWorktreeRoot, "LOCAL_T3_BIN": &c.LocalT3Bin, "LOCAL_CODEX_BIN": &c.LocalCodexBin} {
		if v, ok := env("IB_TICKET_" + k); ok {
			*p = v
		}
	}
	for k, p := range map[string]*int{"VCPUS": &c.VCPUs, "MEM_MIB": &c.Memory, "LOCAL_T3_PORT": &c.LocalT3Port} {
		if v, ok := env("IB_TICKET_" + k); ok {
			n, e := strconv.Atoi(v)
			if e != nil {
				return c, fmt.Errorf("IB_TICKET_%s must be an integer", k)
			}
			*p = n
		}
	}
	if v, ok := env("IB_TICKET_SECRETS"); ok {
		c.Secrets = nil
		for _, s := range strings.Split(v, ",") {
			if strings.TrimSpace(s) != "" {
				c.Secrets = append(c.Secrets, strings.TrimSpace(s))
			}
		}
	}
	return c, c.Validate()
}

func (c Config) Validate() error {
	if c.Host != "" && c.Host != "instabox" && c.Host != "local" {
		return errors.New("host must be instabox or local")
	}
	if c.Backend != "" && c.Backend != "native" && c.Backend != "t3" {
		return errors.New("backend must be native or t3")
	}
	if c.Backend == "t3" && c.Provider != "codex" {
		return errors.New("T3 adapter currently supports Codex only; native Claude remains available")
	}
	if c.Host == "local" {
		if c.Backend != "t3" || c.Provider != "codex" {
			return errors.New("local host requires backend t3 and provider codex")
		}
		for name, value := range map[string]string{"local_repo_root": c.LocalRepoRoot, "local_worktree_root": c.LocalWorktreeRoot} {
			if !filepath.IsAbs(expandHome(value)) {
				return fmt.Errorf("%s must be an absolute path", name)
			}
		}
		if c.LocalT3Bin == "" || c.LocalCodexBin == "" {
			return errors.New("local_t3_bin and local_codex_bin are required")
		}
		if c.LocalT3Port < 1024 || c.LocalT3Port > 65535 {
			return errors.New("local_t3_port must be between 1024 and 65535")
		}
	}
	if c.T3PublicURL != "" && !validT3Origin(c.T3PublicURL) {
		return errors.New("t3_public_url must be an HTTPS origin without a path, credentials or query")
	}
	switch c.CredentialBackend {
	case "", "native":
		if c.CredentialKeyFile != "" {
			return errors.New("credential_key_file requires encrypted-file backend")
		}
	case "encrypted-file":
		if !filepath.IsAbs(expandHome(c.CredentialKeyFile)) {
			return errors.New("encrypted-file backend requires absolute credential_key_file path")
		}
	default:
		return errors.New("credential_backend must be native or encrypted-file")
	}
	for _, s := range []string{c.User, c.Org} {
		if s != "" {
			if e := validID(s); e != nil {
				return e
			}
		}
	}
	if c.Provider != "codex" && c.Provider != "claude" {
		return errors.New("provider must be codex or claude")
	}
	if c.Repo != "" && !repoPattern.MatchString(c.Repo) {
		return errors.New("repo must be OWNER/REPO")
	}
	if c.Base == "" || strings.HasPrefix(c.Base, "-") || strings.ContainsAny(c.Base, "\r\n\x00") {
		return errors.New("invalid base branch")
	}
	if c.AutoSuspend != "" && c.AutoSuspend != "5m" && c.AutoSuspend != "10m" && c.AutoSuspend != "15m" && c.AutoSuspend != "20m" && c.AutoSuspend != "25m" && c.AutoSuspend != "30m" {
		return errors.New("auto_suspend must be 5m, 10m, 15m, 20m, 25m or 30m; free plans require 30m")
	}
	if c.VCPUs <= 0 || c.Memory <= 0 {
		return errors.New("VM size must contain positive integers")
	}
	for _, s := range c.Secrets {
		if !secretPattern.MatchString(s) {
			return errors.New("secrets must contain environment names, never values")
		}
	}
	return nil
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
