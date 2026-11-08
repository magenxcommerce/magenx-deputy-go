package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// commandPath is the PATH of every command deputy starts; the service environment is never inherited.
const commandPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// Identity is the user that commands run as and that owns the files deputy creates.
// The zero value means "the deputy process itself": no privilege change, no chown.
type Identity struct {
	Name   string
	Home   string
	UID    int
	GID    int
	Groups []uint32
	// switchUser is set when deputy runs as root and the identity is another user.
	switchUser bool
}

// resolveIdentity resolves run_as ("user", "user:group", "uid" or "uid:gid").
// An empty spec is the deputy process user.
// Another user than the process user needs deputy to run as root.
func resolveIdentity(spec string) (Identity, error) {
	euid := os.Geteuid()
	if strings.TrimSpace(spec) == "" {
		return currentIdentity()
	}
	userPart, groupPart, hasGroup := strings.Cut(strings.TrimSpace(spec), ":")
	u, err := lookupUser(userPart)
	if err != nil {
		return Identity{}, fmt.Errorf("run_as %q: %w", spec, err)
	}
	id := Identity{Name: u.Username, Home: u.HomeDir}
	if id.UID, err = strconv.Atoi(u.Uid); err != nil {
		return Identity{}, fmt.Errorf("run_as %q: uid %q: %w", spec, u.Uid, err)
	}
	if id.GID, err = strconv.Atoi(u.Gid); err != nil {
		return Identity{}, fmt.Errorf("run_as %q: gid %q: %w", spec, u.Gid, err)
	}
	if hasGroup {
		g, err := lookupGroup(groupPart)
		if err != nil {
			return Identity{}, fmt.Errorf("run_as %q: %w", spec, err)
		}
		if id.GID, err = strconv.Atoi(g.Gid); err != nil {
			return Identity{}, fmt.Errorf("run_as %q: gid %q: %w", spec, g.Gid, err)
		}
	}
	// supplementary groups, e.g. docker; a numeric uid without passwd entry has none
	if gids, err := u.GroupIds(); err == nil {
		for _, g := range gids {
			if n, err := strconv.ParseUint(g, 10, 32); err == nil && int(n) != id.GID {
				id.Groups = append(id.Groups, uint32(n))
			}
		}
	}
	if id.UID == euid && (!hasGroup || id.GID == os.Getegid()) {
		return id, nil
	}
	if euid != 0 {
		return Identity{}, fmt.Errorf("run_as %q needs deputy to run as root (running as uid %d)", spec, euid)
	}
	id.switchUser = true
	return id, nil
}

func currentIdentity() (Identity, error) {
	id := Identity{UID: os.Geteuid(), GID: os.Getegid()}
	if u, err := user.LookupId(strconv.Itoa(id.UID)); err == nil {
		id.Name = u.Username
		id.Home = u.HomeDir
	}
	if id.Home == "" {
		id.Home = os.Getenv("HOME")
	}
	return id, nil
}

// lookupUser accepts a user name or a numeric uid; a numeric uid without passwd entry is allowed
// (e.g. 165532, the host uid of the userns-remapped container user).
func lookupUser(name string) (*user.User, error) {
	if name == "" {
		return nil, errors.New("empty user")
	}
	if u, err := user.Lookup(name); err == nil {
		return u, nil
	}
	if _, err := strconv.Atoi(name); err != nil {
		return nil, fmt.Errorf("unknown user %q", name)
	}
	if u, err := user.LookupId(name); err == nil {
		return u, nil
	}
	return &user.User{Uid: name, Gid: name, Username: name, HomeDir: "/"}, nil
}

func lookupGroup(name string) (*user.Group, error) {
	if name == "" {
		return nil, errors.New("empty group")
	}
	if g, err := user.LookupGroup(name); err == nil {
		return g, nil
	}
	if _, err := strconv.Atoi(name); err != nil {
		return nil, fmt.Errorf("unknown group %q", name)
	}
	return &user.Group{Gid: name, Name: name}, nil
}

func (id Identity) isRoot() bool { return id.UID == 0 }

func (id Identity) String() string {
	if id.Name != "" {
		return fmt.Sprintf("%s (%d:%d)", id.Name, id.UID, id.GID)
	}
	return fmt.Sprintf("%d:%d", id.UID, id.GID)
}

// own sets the owner of a file, directory or symlink (not its target) deputy created as root.
func (id Identity) own(path string) error {
	if !id.switchUser {
		return nil
	}
	if err := os.Lchown(path, id.UID, id.GID); err != nil {
		return fmt.Errorf("chown %s: %w", path, err)
	}
	return nil
}

// mkdirAll is os.MkdirAll that hands every directory it creates to the identity.
func (id Identity) mkdirAll(path string, perm os.FileMode) error {
	path = filepath.Clean(path)
	var missing []string
	for p := path; ; p = filepath.Dir(p) {
		if _, err := os.Lstat(p); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		missing = append(missing, p)
		if filepath.Dir(p) == p {
			break
		}
	}
	if err := os.MkdirAll(path, perm); err != nil {
		return err
	}
	for i := len(missing) - 1; i >= 0; i-- {
		if err := id.own(missing[i]); err != nil {
			return err
		}
	}
	return nil
}

// symlink atomically points linkPath to target, the link owned by the identity.
func (id Identity) symlink(target, linkPath string) error {
	if err := id.mkdirAll(filepath.Dir(linkPath), 0o750); err != nil {
		return err
	}
	tmp := linkPath + ".next"
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := id.own(tmp); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, linkPath)
}

// Runner starts commands as an identity with a clean environment: HOME, USER, LOGNAME, PATH and the
// configured extra variables. The deputy environment (S3 keys, registry password, ...) is not passed on.
type Runner struct {
	ID  Identity
	Env []string
}

// newRunner builds the base environment. Each entry of extra is KEY=VALUE, or a bare NAME that passes
// the value deputy itself has for NAME.
func newRunner(id Identity, extra []string) Runner {
	env := []string{"HOME=" + id.Home, "PATH=" + commandPath}
	if id.Name != "" {
		env = append(env, "USER="+id.Name, "LOGNAME="+id.Name)
	}
	if lang := os.Getenv("LANG"); lang != "" {
		env = append(env, "LANG="+lang)
	}
	for _, e := range extra {
		if strings.Contains(e, "=") {
			env = append(env, e)
		} else if v, ok := os.LookupEnv(e); ok {
			env = append(env, e+"="+v)
		}
	}
	return Runner{ID: id, Env: env}
}

func (r Runner) command(ctx context.Context, dir string, env []string, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(append([]string{}, r.Env...), env...)
	if r.ID.switchUser {
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Credential: &syscall.Credential{Uid: uint32(r.ID.UID), Gid: uint32(r.ID.GID), Groups: r.ID.Groups},
		}
	}
	return cmd
}

// Output runs a command and returns its stdout; stderr is part of the error.
func (r Runner) Output(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	cmd := r.command(ctx, dir, env, name, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return out, fmt.Errorf("%s: %w: %s", name, err, msg)
		}
		return out, fmt.Errorf("%s: %w", name, err)
	}
	return out, nil
}

// Run runs a command, its combined output is part of the error.
func (r Runner) Run(ctx context.Context, dir string, env []string, name string, args ...string) error {
	output, err := r.command(ctx, dir, env, name, args...).CombinedOutput()
	if err != nil {
		if msg := strings.TrimSpace(string(output)); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}

// Shell runs command with /bin/sh in releaseDir. {release_dir} is replaced by the shell-quoted release
// path, which is also exported as RELEASE_DIR.
func (r Runner) Shell(ctx context.Context, command, releaseDir string) error {
	expanded := strings.ReplaceAll(command, "{release_dir}", shellQuote(releaseDir))
	return r.Run(ctx, releaseDir, []string{"RELEASE_DIR=" + releaseDir}, "/bin/sh", "-c", expanded)
}
