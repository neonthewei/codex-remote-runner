package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func headlessStore(t *testing.T) *EncryptedSecrets {
	t.Helper()
	root := t.TempDir()
	key := filepath.Join(root, "keys", "master.key")
	if e := initCredentialKey(key); e != nil {
		t.Fatal(e)
	}
	return &EncryptedSecrets{KeyFile: key, Dir: filepath.Join(root, "state", "credentials"), Namespace: "namespace-one"}
}

func TestHeadlessStoreRestartAndImmutableVersions(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "disabled-for-headless-test")
	s := headlessStore(t)
	if e := s.Put("owner", "version-one", fakeAuth); e != nil {
		t.Fatal(e)
	}
	path, _, e := s.location("owner", "version-one")
	if e != nil {
		t.Fatal(e)
	}
	encrypted, e := os.ReadFile(path)
	if e != nil || bytes.Contains(encrypted, fakeAuth) || bytes.Contains(encrypted, []byte("fake-access")) {
		t.Fatal("credential plaintext found in encrypted file")
	}
	if e = s.Put("owner", "version-one", fakeAuth); e == nil {
		t.Fatal("immutable version was overwritten")
	}
	restarted := &EncryptedSecrets{KeyFile: s.KeyFile, Dir: s.Dir, Namespace: s.Namespace}
	got, e := restarted.Get("owner", "version-one")
	if e != nil || !bytes.Equal(got, fakeAuth) {
		t.Fatal("headless restart could not restore credential")
	}
}

func TestHeadlessKeyMustRemainOutsideBundleDirectory(t *testing.T) {
	s := headlessStore(t)
	s.KeyFile = filepath.Join(s.Dir, "master.key")
	if e := initCredentialKey(s.KeyFile); e != nil {
		t.Fatal(e)
	}
	if e := s.Put("owner", "one", fakeAuth); e == nil {
		t.Fatal("key stored alongside encrypted bundles")
	}
}

func TestHeadlessStoreRejectsTamperingWrongKeyAndSubstitution(t *testing.T) {
	for _, fault := range []string{"tampered", "wrong-key", "other-user", "other-version", "other-namespace", "bad-header"} {
		t.Run(fault, func(t *testing.T) {
			s := headlessStore(t)
			if e := s.Put("owner", "one", fakeAuth); e != nil {
				t.Fatal(e)
			}
			path, _, _ := s.location("owner", "one")
			raw, _ := os.ReadFile(path)
			user, version := "owner", "one"
			switch fault {
			case "tampered":
				raw[len(raw)-1] ^= 1
			case "wrong-key":
				s.KeyFile = headlessStore(t).KeyFile
			case "other-user":
				user = "other"
			case "other-version":
				version = "two"
			case "other-namespace":
				s.Namespace = "namespace-two"
			case "bad-header":
				raw[0] ^= 1
			}
			target, _, _ := s.location(user, version)
			if e := os.WriteFile(target, raw, 0600); e != nil {
				t.Fatal(e)
			}
			if got, e := s.Get(user, version); e == nil || len(got) != 0 {
				t.Fatal("untrusted credential accepted")
			}
		})
	}
}

func TestHeadlessStoreRejectsUnsafeFiles(t *testing.T) {
	for _, fault := range []string{"missing-key", "short-key", "public-key", "symlink-key", "public-data", "symlink-data", "public-directory"} {
		t.Run(fault, func(t *testing.T) {
			s := headlessStore(t)
			if e := s.Put("owner", "one", fakeAuth); e != nil {
				t.Fatal(e)
			}
			path, _, _ := s.location("owner", "one")
			switch fault {
			case "missing-key":
				os.Remove(s.KeyFile)
			case "short-key":
				os.WriteFile(s.KeyFile, []byte("short"), 0600)
			case "public-key":
				os.Chmod(s.KeyFile, 0644)
			case "symlink-key":
				original := s.KeyFile
				s.KeyFile += ".link"
				if e := os.Symlink(original, s.KeyFile); e != nil {
					t.Fatal(e)
				}
			case "public-data":
				os.Chmod(path, 0644)
			case "symlink-data":
				if e := os.Rename(path, path+".original"); e != nil {
					t.Fatal(e)
				}
				if e := os.Symlink(path+".original", path); e != nil {
					t.Fatal(e)
				}
			case "public-directory":
				os.Chmod(s.Dir, 0755)
			}
			if _, e := s.Get("owner", "one"); e == nil {
				t.Fatal("unsafe credential location accepted")
			}
		})
	}
}

func TestHeadlessCheckpointAndLostKeyPreserveLease(t *testing.T) {
	r, f, _ := setup(t)
	seedTask(t, r, f)
	s := headlessStore(t)
	r.Secrets = s
	if _, e := r.Checkpoint("TASK-1"); e != nil {
		t.Fatal(e)
	}
	if f.removed != 1 {
		t.Fatal("successful checkpoint did not remove guest auth")
	}
	r.Secrets = &EncryptedSecrets{KeyFile: s.KeyFile, Dir: s.Dir, Namespace: s.Namespace}
	if _, e := r.Activate("TASK-1"); e != nil || f.uploads != 1 {
		t.Fatal("restart restore failed", e)
	}
	if _, e := r.Checkpoint("TASK-1"); e != nil {
		t.Fatal(e)
	}
	if e := os.Remove(s.KeyFile); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Activate("TASK-1"); e == nil || f.uploads != 1 {
		t.Fatal("missing key ignored")
	}
	if r.State.Acquire("owner", "OTHER") == nil {
		t.Fatal("failed restore released profile lease")
	}
}

func TestInitKeyNeverOverwritesOrPrintsKey(t *testing.T) {
	s := headlessStore(t)
	before, _ := os.ReadFile(s.KeyFile)
	if e := initCredentialKey(s.KeyFile); e == nil {
		t.Fatal("replaced existing master key")
	}
	after, _ := os.ReadFile(s.KeyFile)
	if len(before) != 32 || !bytes.Equal(before, after) {
		t.Fatal("master key changed")
	}
	newKey := filepath.Join(t.TempDir(), "private", "master.key")
	t.Setenv("CODEX_REMOTE_CONFIG", "")
	t.Setenv("CODEX_REMOTE_STATE", "/nonexistent/no-state-should-be-opened")
	result, e := execute([]string{"init-key", "--credential-backend", "encrypted-file", "--credential-key-file", newKey})
	if e != nil || result.(map[string]string)["status"] != "key-created" {
		t.Fatal("key initialization failed", e)
	}
}

func TestHeadlessConfigUsesOnlyBackendAndKeyReference(t *testing.T) {
	key := filepath.Join(t.TempDir(), "keys", "master.key")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if e := os.WriteFile(path, []byte("credential_backend: encrypted-file\ncredential_key_file: "+key+"\n"), 0600); e != nil {
		t.Fatal(e)
	}
	c, e := loadConfig(path, func(string) (string, bool) { return "", false })
	if e != nil || c.CredentialBackend != "encrypted-file" || c.CredentialKeyFile != key {
		t.Fatal("headless configuration failed", e)
	}
	for _, value := range []string{"credential_backend: unknown", "credential_backend: encrypted-file", "credential_key_file: fake-secret", "credential_backend: encrypted-file\ncredential_key: do-not-echo"} {
		if e := os.WriteFile(path, []byte(value), 0600); e != nil {
			t.Fatal(e)
		}
		if _, e := loadConfig(path, func(string) (string, bool) { return "", false }); e == nil || strings.Contains(e.Error(), "do-not-echo") {
			t.Fatal("unsafe configuration accepted or leaked")
		}
	}
}
