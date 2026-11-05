package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"

	"github.com/99designs/keyring"
	"github.com/gofrs/flock"
	_ "modernc.org/sqlite"
)

func randomID() string {
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		panic("system randomness unavailable")
	}
	return hex.EncodeToString(b)
}

type Task struct {
	Backend      string `json:"backend,omitempty"`
	Ticket       string `json:"ticket"`
	User         string `json:"user"`
	Provider     string `json:"provider"`
	Org          string `json:"org"`
	Name         string `json:"name"`
	Box          string `json:"box"`
	Phase        string `json:"phase"`
	Version      string `json:"version,omitempty"`
	Conversation string `json:"conversation,omitempty"`
	PR           string `json:"pr,omitempty"`
	Repo         string `json:"repo,omitempty"`
	Base         string `json:"base,omitempty"`
	Branch       string `json:"branch,omitempty"`
	Adopted      int    `json:"adopted"`
	Host         string `json:"host,omitempty"`
	SourceRoot   string `json:"source_root,omitempty"`
	Worktree     string `json:"worktree,omitempty"`
	T3Home       string `json:"t3_home,omitempty"`
	T3Port       int    `json:"t3_port,omitempty"`
	Owner        string `json:"owner,omitempty"`
}

type State struct {
	DB        *sql.DB
	lock      *flock.Flock
	Namespace string
	Root      string
}

func openState(root string) (*State, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, errors.New("cannot resolve state directory")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("state directory must have mode 0700")
	}
	l := flock.New(filepath.Join(root, "controller.lock"))
	ok, err := l.TryLock()
	if err != nil || !ok {
		return nil, errors.New("controller busy")
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "state.db"))
	if err != nil {
		l.Unlock()
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &State{DB: db, lock: l, Root: root}
	fail := func(e error) (*State, error) { s.Close(); return nil, e }
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS metadata(key TEXT PRIMARY KEY,value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS profiles(user TEXT PRIMARY KEY,version TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS tasks(ticket TEXT PRIMARY KEY,user TEXT NOT NULL,provider TEXT NOT NULL,org TEXT NOT NULL,name TEXT UNIQUE NOT NULL,box TEXT UNIQUE,phase TEXT NOT NULL,version TEXT,conversation TEXT,pr TEXT,repo TEXT,base TEXT,branch TEXT,adopted INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS leases(user TEXT PRIMARY KEY,ticket TEXT NOT NULL UNIQUE);
CREATE TABLE IF NOT EXISTS turns(ticket TEXT PRIMARY KEY,conversation TEXT NOT NULL,job TEXT NOT NULL,mode TEXT NOT NULL,status TEXT NOT NULL,note TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS t3_tasks(ticket TEXT PRIMARY KEY, model TEXT NOT NULL DEFAULT '', link TEXT NOT NULL DEFAULT '', nonce TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS local_tasks(ticket TEXT PRIMARY KEY,source_root TEXT NOT NULL,worktree TEXT NOT NULL,t3_home TEXT NOT NULL,t3_port INTEGER NOT NULL,owner TEXT NOT NULL);`)
	if err != nil {
		return fail(err)
	}
	if err = os.Chmod(filepath.Join(root, "state.db"), 0600); err != nil {
		return fail(err)
	}
	if _, err = db.Exec("INSERT OR IGNORE INTO metadata VALUES('namespace',?)", randomID()); err != nil {
		return fail(err)
	}
	if err = db.QueryRow("SELECT value FROM metadata WHERE key='namespace'").Scan(&s.Namespace); err != nil {
		return fail(err)
	}
	return s, nil
}

func (s *State) Close() { s.DB.Close(); s.lock.Unlock() }
func (s *State) Task(ticket string) (Task, error) {
	t := Task{}
	err := s.DB.QueryRow(`SELECT ticket,user,provider,org,name,coalesce(box,''),phase,coalesce(version,''),coalesce(conversation,''),coalesce(pr,''),coalesce(repo,''),coalesce(base,''),coalesce(branch,''),adopted FROM tasks WHERE ticket=?`, ticket).Scan(&t.Ticket, &t.User, &t.Provider, &t.Org, &t.Name, &t.Box, &t.Phase, &t.Version, &t.Conversation, &t.PR, &t.Repo, &t.Base, &t.Branch, &t.Adopted)
	if err == sql.ErrNoRows {
		return t, errors.New("unknown task")
	}
	if err == nil {
		var n int
		err = s.DB.QueryRow("SELECT count(*) FROM t3_tasks WHERE ticket=?", ticket).Scan(&n)
		if n == 1 {
			t.Backend = "t3"
		}
		err = s.DB.QueryRow("SELECT source_root,worktree,t3_home,t3_port,owner FROM local_tasks WHERE ticket=?", ticket).Scan(&t.SourceRoot, &t.Worktree, &t.T3Home, &t.T3Port, &t.Owner)
		if err == nil {
			t.Host = "local"
		} else if err == sql.ErrNoRows {
			t.Host = "instabox"
			err = nil
		}
	}
	return t, err
}
func (s *State) Save(t Task) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if t.Backend == "t3" {
		if _, e := tx.Exec("INSERT OR IGNORE INTO t3_tasks(ticket) VALUES(?)", t.Ticket); e != nil {
			return e
		}
	}
	if t.Host == "local" {
		if _, e := tx.Exec("INSERT INTO local_tasks(ticket,source_root,worktree,t3_home,t3_port,owner) VALUES(?,?,?,?,?,?) ON CONFLICT(ticket) DO UPDATE SET source_root=excluded.source_root,worktree=excluded.worktree,t3_home=excluded.t3_home,t3_port=excluded.t3_port,owner=excluded.owner", t.Ticket, t.SourceRoot, t.Worktree, t.T3Home, t.T3Port, t.Owner); e != nil {
			return e
		}
	}
	_, e := tx.Exec(`INSERT INTO tasks(ticket,user,provider,org,name,box,phase,version,conversation,pr,repo,base,branch,adopted) VALUES(?,?,?,?,?,nullif(?,''),?,nullif(?,''),nullif(?,''),nullif(?,''),nullif(?,''),nullif(?,''),nullif(?,''),?) ON CONFLICT(ticket) DO UPDATE SET box=excluded.box,phase=excluded.phase,version=excluded.version,conversation=excluded.conversation,pr=excluded.pr,repo=excluded.repo,base=excluded.base,branch=excluded.branch`, t.Ticket, t.User, t.Provider, t.Org, t.Name, t.Box, t.Phase, t.Version, t.Conversation, t.PR, t.Repo, t.Base, t.Branch, t.Adopted)
	if e != nil {
		return e
	}
	return tx.Commit()
}
func (s *State) Version(user string) (string, error) {
	var v string
	e := s.DB.QueryRow("SELECT version FROM profiles WHERE user=?", user).Scan(&v)
	if e == sql.ErrNoRows {
		return "", nil
	}
	return v, e
}
func (s *State) Acquire(user, ticket string) error {
	var other string
	e := s.DB.QueryRow("SELECT ticket FROM leases WHERE user=?", user).Scan(&other)
	if e != nil && e != sql.ErrNoRows {
		return e
	}
	if e == nil && other != ticket {
		return errors.New("Codex profile busy with another task; wait for its checkpoint")
	}
	_, e = s.DB.Exec("INSERT OR IGNORE INTO leases VALUES(?,?)", user, ticket)
	return e
}
func (s *State) CheckpointVersion(t *Task, version string) error {
	tx, e := s.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var current string
	e = tx.QueryRow("SELECT version FROM profiles WHERE user=?", t.User).Scan(&current)
	if e != nil && e != sql.ErrNoRows {
		return e
	}
	if current != t.Version {
		return errors.New("stale credential version; refusing overwrite")
	}
	if _, e = tx.Exec("INSERT INTO profiles VALUES(?,?) ON CONFLICT(user) DO UPDATE SET version=excluded.version", t.User, version); e != nil {
		return e
	}
	if _, e = tx.Exec("UPDATE tasks SET version=?,phase='auth-saved' WHERE ticket=?", version, t.Ticket); e != nil {
		return e
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	t.Version = version
	t.Phase = "auth-saved"
	return nil
}
func (s *State) Release(t *Task) error {
	tx, e := s.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.Exec("UPDATE tasks SET phase='paused' WHERE ticket=?", t.Ticket); e != nil {
		return e
	}
	if _, e = tx.Exec("DELETE FROM leases WHERE ticket=?", t.Ticket); e != nil {
		return e
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	t.Phase = "paused"
	return nil
}

func validateAuth(raw []byte) ([]byte, error) {
	var v struct {
		Mode   string         `json:"auth_mode"`
		Tokens map[string]any `json:"tokens"`
		APIKey any            `json:"OPENAI_API_KEY"`
	}
	if json.Unmarshal(raw, &v) != nil || v.Mode != "chatgpt" || (v.APIKey != nil && v.APIKey != "") {
		return nil, errors.New("expected ChatGPT-managed Codex auth")
	}
	for _, k := range []string{"access_token", "refresh_token", "id_token"} {
		s, ok := v.Tokens[k].(string)
		if !ok || s == "" {
			return nil, errors.New("expected complete ChatGPT-managed Codex auth")
		}
	}
	return raw, nil
}

type Secrets interface {
	Get(string, string) ([]byte, error)
	Put(string, string, []byte) error
}
type OSSecrets struct {
	service string
	ring    keyring.Keyring
}

func (o *OSSecrets) open() error {
	if o.ring != nil {
		return nil
	}
	r, e := keyring.Open(keyring.Config{
		ServiceName:              o.service,
		AllowedBackends:          []keyring.BackendType{keyring.KeychainBackend, keyring.SecretServiceBackend},
		KeychainTrustApplication: true,
		// Use the operator's existing unlocked GNOME collection. Creating a new
		// collection for each state namespace requires a graphical password prompt.
		LibSecretCollectionName: "login",
	})
	if e != nil {
		return errors.New("OS credential store unavailable; plaintext fallback is disabled")
	}
	o.ring = r
	return nil
}

func (o *OSSecrets) key(user, version string) string {
	key := "codex/" + user + "/" + version
	// Secret Service scopes items to a collection, unlike Keychain's service
	// attribute. Preserve namespace isolation within the shared login collection.
	if runtime.GOOS == "linux" {
		return o.service + "/" + key
	}
	return key
}
func (o *OSSecrets) Get(user, version string) ([]byte, error) {
	if e := o.open(); e != nil {
		return nil, e
	}
	v, e := o.ring.Get(o.key(user, version))
	if e != nil {
		return nil, errors.New("saved Codex login unavailable or OS access denied; verify native credential-store access before reseeding")
	}
	return validateAuth(v.Data)
}
func (o *OSSecrets) Put(user, version string, value []byte) error {
	if _, e := validateAuth(value); e != nil {
		return e
	}
	if e := o.open(); e != nil {
		return e
	}
	if e := o.ring.Set(keyring.Item{Key: o.key(user, version), Data: value, Label: "Instabox coding login"}); e != nil {
		return errors.New("OS credential store write failed")
	}
	return nil
}
