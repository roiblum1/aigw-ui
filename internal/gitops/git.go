// Package gitops keeps the models the hub deploys as files in a git
// repository, which Argo CD deploys: one values file of the model-deployer
// chart per model and cluster (<dir>/deployments/<cluster>/<name>.yaml), and
// presets to start from (<dir>/presets/<name>.yaml).
//
// The repository is the source of truth, not the database: the hub reads the
// files back on every look, so a change made by hand in git shows in the UI,
// and it writes a file only when someone saves that deployment. It works with
// any git server through the git command, over HTTPS with a token or over
// SSH with a key.
package gitops

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Config says where the files are and how the hub may push.
type Config struct {
	// Repo is the repository's URL (https://, ssh:// or git@host:path, or
	// file:// in tests). Empty turns deployments off.
	Repo string
	// Branch is the branch Argo CD reads; the hub commits onto it.
	Branch string
	// Dir is the directory in the repository that holds deployments/ and
	// presets/.
	Dir string
	// Username and Token authenticate over HTTPS. The token is sent as the
	// password of HTTP basic authentication, which GitHub and GitLab take.
	Username, Token string
	// SSHKeyFile and KnownHostsFile authenticate over SSH. Without a known
	// hosts file the server's key is not checked.
	SSHKeyFile, KnownHostsFile string
	// AuthorName and AuthorEmail sign the commits.
	AuthorName, AuthorEmail string
	// WorkDir holds the clone. It must be writable.
	WorkDir string
}

// fetchEvery is how long a read uses the last fetch before it asks the
// server again: a page polling every few seconds does not hit git each time.
const fetchEvery = 10 * time.Second

// gitTimeout bounds one git command.
const gitTimeout = 60 * time.Second

// ErrConflict is a write over a file that changed since the caller read it,
// or a new file where one exists.
var ErrConflict = errors.New("the file changed in git since it was read")

// Repo is the hub's clone of the repository. One git operation runs at a
// time.
type Repo struct {
	cfg       Config
	mu        sync.Mutex
	dir       string
	env       []string
	fetchedAt time.Time
}

// New prepares the clone's directory and the credentials. It does not
// contact the server; the first read clones.
func New(cfg Config) (*Repo, error) {
	if cfg.Repo == "" {
		return nil, errors.New("no repository")
	}
	if cfg.Branch == "" {
		cfg.Branch = "main"
	}
	cfg.Dir = strings.Trim(cfg.Dir, "/")
	if cfg.AuthorName == "" {
		cfg.AuthorName = "aigw-ui"
	}
	if cfg.AuthorEmail == "" {
		cfg.AuthorEmail = "aigw-ui@localhost"
	}
	if cfg.WorkDir == "" {
		cfg.WorkDir = filepath.Join(os.TempDir(), "aigw-ui-gitops")
	}
	if err := os.MkdirAll(cfg.WorkDir, 0o700); err != nil {
		return nil, fmt.Errorf("git work directory: %w", err)
	}
	r := &Repo{cfg: cfg, dir: filepath.Join(cfg.WorkDir, "clone")}
	// git reads $HOME for its configuration; OpenShift's random user has /.
	r.env = append(os.Environ(), "HOME="+cfg.WorkDir, "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1")
	if cfg.Token != "" {
		user := cfg.Username
		if user == "" {
			user = "git"
		}
		// In the environment, not on the command line, where any process
		// on the node could read it.
		auth := base64.StdEncoding.EncodeToString([]byte(user + ":" + cfg.Token))
		r.env = append(r.env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=Authorization: Basic "+auth)
	}
	if cfg.SSHKeyFile != "" {
		// ssh refuses a key others can read, and a mounted Secret is
		// readable by its group: it uses a copy only this user can read.
		key, err := os.ReadFile(cfg.SSHKeyFile)
		if err != nil {
			return nil, fmt.Errorf("SSH key: %w", err)
		}
		keyFile := filepath.Join(cfg.WorkDir, "ssh-key")
		if err := os.WriteFile(keyFile, key, 0o600); err != nil {
			return nil, fmt.Errorf("SSH key: %w", err)
		}
		ssh := "ssh -i " + keyFile + " -o IdentitiesOnly=yes -o BatchMode=yes"
		if cfg.KnownHostsFile != "" {
			ssh += " -o StrictHostKeyChecking=yes -o UserKnownHostsFile=" + cfg.KnownHostsFile
		} else {
			ssh += " -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null"
		}
		r.env = append(r.env, "GIT_SSH_COMMAND="+ssh)
	}
	return r, nil
}

// Where returns the repository, branch and directory, for display.
func (r *Repo) Where() (repo, branch, dir string) {
	return r.cfg.Repo, r.cfg.Branch, r.cfg.Dir
}

// git runs one git command in the clone and returns its output.
func (r *Repo) git(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = r.dir
	cmd.Env = r.env
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errOut.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.String(), fmt.Errorf("git %s: %s", args[0], r.redact(msg))
	}
	return out.String(), nil
}

// redact keeps the token out of an error that quotes a URL.
func (r *Repo) redact(s string) string {
	if r.cfg.Token != "" {
		s = strings.ReplaceAll(s, r.cfg.Token, "***")
	}
	return s
}

// sync brings the clone to the branch's head on the server, cloning it the
// first time. Local changes are dropped. force skips the fetchEvery wait.
func (r *Repo) sync(ctx context.Context, force bool) error {
	if !force && time.Since(r.fetchedAt) < fetchEvery {
		return nil
	}
	if _, err := os.Stat(filepath.Join(r.dir, ".git")); err != nil {
		if err := os.MkdirAll(r.dir, 0o700); err != nil {
			return err
		}
		if _, err := r.git(ctx, "init", "-q"); err != nil {
			return err
		}
		if _, err := r.git(ctx, "remote", "add", "origin", r.cfg.Repo); err != nil {
			return err
		}
	}
	// Only the head is needed: the hub reads the current files and commits
	// on top of them.
	if _, err := r.git(ctx, "fetch", "-q", "--depth", "1", "origin", r.cfg.Branch); err != nil {
		return err
	}
	if _, err := r.git(ctx, "checkout", "-q", "-B", r.cfg.Branch, "FETCH_HEAD"); err != nil {
		return err
	}
	if _, err := r.git(ctx, "reset", "-q", "--hard", "FETCH_HEAD"); err != nil {
		return err
	}
	if _, err := r.git(ctx, "clean", "-q", "-fdx"); err != nil {
		return err
	}
	r.fetchedAt = time.Now()
	return nil
}

// head is the commit the clone is at.
func (r *Repo) head(ctx context.Context) (string, error) {
	out, err := r.git(ctx, "rev-parse", "HEAD")
	return strings.TrimSpace(out), err
}

// blob is a file's git object ID at the clone's head, "" when it does not
// exist. It is what a caller passes back to say which version it changes.
func (r *Repo) blob(ctx context.Context, path string) (string, error) {
	out, err := r.git(ctx, "ls-tree", "HEAD", "--", path)
	if err != nil {
		return "", err
	}
	// <mode> blob <id>\t<path>
	f := strings.Fields(out)
	if len(f) < 3 {
		return "", nil
	}
	return f[2], nil
}

// Change is one file to write or, with Content nil, to remove. With Dir
// set the whole directory of Path is removed (a platform registration).
type Change struct {
	// Path is relative to Config.Dir.
	Path    string
	Content []byte
	Dir     bool
	// Base is the blob the caller read ("" for a new file). A file whose
	// blob is no longer Base is not touched: ErrConflict.
	Base string
}

// pushTries is how often a commit is made again on a newer head when
// someone else pushed in between.
const pushTries = 3

// Commit writes the changes as one commit and pushes it. It returns the
// commit. Nothing is written when any file changed since its Base.
func (r *Repo) Commit(ctx context.Context, changes []Change, message string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var err error
	for range pushTries {
		if err = r.sync(ctx, true); err != nil {
			return "", err
		}
		for _, c := range changes {
			path := r.path(c.Path)
			var blob string
			if blob, err = r.blob(ctx, path); err != nil {
				return "", err
			}
			if blob != c.Base {
				return "", ErrConflict
			}
		}
		for _, c := range changes {
			if err = r.apply(ctx, c); err != nil {
				return "", err
			}
		}
		if _, err = r.git(ctx, "-c", "user.name="+r.cfg.AuthorName, "-c", "user.email="+r.cfg.AuthorEmail,
			"commit", "-q", "-m", message); err != nil {
			return "", err
		}
		if _, err = r.git(ctx, "push", "-q", "origin", "HEAD:refs/heads/"+r.cfg.Branch); err == nil {
			return r.head(ctx)
		}
		// Someone pushed first: start again from their head.
		r.fetchedAt = time.Time{}
	}
	return "", err
}

// path is a path relative to Config.Dir as a path in the repository.
func (r *Repo) path(rel string) string {
	return filepath.ToSlash(filepath.Join(r.cfg.Dir, rel))
}

// apply makes one change in the clone and stages it.
func (r *Repo) apply(ctx context.Context, c Change) error {
	path := r.path(c.Path)
	full := filepath.Join(r.dir, filepath.FromSlash(path))
	switch {
	case c.Dir:
		_, err := r.git(ctx, "rm", "-q", "-r", "--", filepath.ToSlash(filepath.Dir(path)))
		return err
	case c.Content == nil:
		_, err := r.git(ctx, "rm", "-q", "--", path)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(full, c.Content, 0o600); err != nil {
		return err
	}
	_, err := r.git(ctx, "add", "--", path)
	return err
}
