package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func writeTarGz(t *testing.T, path string, headers []*tar.Header) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, h := range headers {
		if h.Typeflag == 0 {
			h.Typeflag = tar.TypeReg
		}
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	writeFile(t, path, buf.String(), 0o644)
}

func TestCheckArchive(t *testing.T) {
	tests := []struct {
		name    string
		strip   int
		headers []*tar.Header
		want    string
	}{
		{"plain", 0, []*tar.Header{{Name: "app/", Typeflag: tar.TypeDir, Mode: 0o755}, {Name: "app/index.php"}, {Name: "vendor/bin/x", Typeflag: tar.TypeSymlink, Linkname: "../pkg/bin/x"}}, ""},
		{"strip top dir", 1, []*tar.Header{{Name: "repo-sha/", Typeflag: tar.TypeDir, Mode: 0o755}, {Name: "repo-sha/a", Typeflag: tar.TypeLink, Linkname: "repo-sha/b"}}, ""},
		{"absolute", 0, []*tar.Header{{Name: "/etc/passwd"}}, "absolute path"},
		{"dot dot", 0, []*tar.Header{{Name: "../x"}}, "leaves the release"},
		{"setuid", 0, []*tar.Header{{Name: "bin/x", Mode: 0o4755}}, "setuid"},
		{"absolute symlink", 0, []*tar.Header{{Name: "var", Typeflag: tar.TypeSymlink, Linkname: "/etc"}}, "symlink to absolute path"},
		{"escaping symlink", 0, []*tar.Header{{Name: "a/var", Typeflag: tar.TypeSymlink, Linkname: "../../etc"}}, "leaves the release"},
		{"escaping hard link", 0, []*tar.Header{{Name: "x", Typeflag: tar.TypeLink, Linkname: "../etc/shadow"}}, "hard link"},
		{"device", 0, []*tar.Header{{Name: "dev", Typeflag: tar.TypeChar}}, "unsupported entry type"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "a.tar.gz")
			writeTarGz(t, path, tc.headers)
			err := checkArchive(path, tc.strip)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v want %q", err, tc.want)
			}
		})
	}
}

func TestLinkSharedPathsRefusesSymlinkOutsideRelease(t *testing.T) {
	release := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(release, "var")); err != nil {
		t.Fatal(err)
	}
	err := linkSharedPaths(Identity{}, release, []SharedTarget{{Source: "/shared/var/export", Target: "var/export"}})
	if err == nil || !strings.Contains(err.Error(), "outside the release") {
		t.Fatalf("err=%v", err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("wrote outside the release: %v", entries)
	}
}

func TestRunnerCleanEnvironment(t *testing.T) {
	t.Setenv("DEPUTY_TEST_SECRET", "s3cr3t")
	t.Setenv("DEPUTY_TEST_PASS", "passed")
	id, _ := currentIdentity()
	r := newRunner(id, []string{"DEPUTY_TEST_PASS", "FIXED=1"})
	out, err := r.Output(context.Background(), t.TempDir(), nil, "/bin/sh", "-c", `echo "$DEPUTY_TEST_SECRET|$DEPUTY_TEST_PASS|$FIXED|$PATH"`)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != "|passed|1|"+commandPath {
		t.Fatalf("env=%q", got)
	}
}

func TestResolveIdentity(t *testing.T) {
	id, err := resolveIdentity("")
	if err != nil || id.switchUser || int64(id.UID) != int64(os.Geteuid()) {
		t.Fatalf("empty run_as: %+v %v", id, err)
	}
	self, err := resolveIdentity(strconv.Itoa(os.Geteuid()))
	if err != nil || self.switchUser {
		t.Fatalf("own uid: %+v %v", self, err)
	}
	if _, err := resolveIdentity("no-such-user-deputy"); err == nil {
		t.Fatal("unknown user accepted")
	}
	// negative, signed or out-of-range ids would wrap when converted to uint32
	for _, spec := range []string{"-1", "+0", "4294967295", "4294967296", "0:-1", "0:4294967295"} {
		if _, err := resolveIdentity(spec); err == nil {
			t.Errorf("run_as %q accepted", spec)
		}
	}
	other := "nobody"
	if os.Geteuid() != 0 {
		if _, err := resolveIdentity(other); err == nil || !strings.Contains(err.Error(), "needs deputy to run as root") {
			t.Fatalf("non-root switch: err=%v", err)
		}
		return
	}
	id, err = resolveIdentity(other)
	if err != nil || !id.switchUser || id.UID == 0 {
		t.Fatalf("root -> nobody: %+v %v", id, err)
	}
	// numeric uid without passwd entry, e.g. the userns-remapped container user
	id, err = resolveIdentity("165532:165532")
	if err != nil || id.UID != 165532 || id.GID != 165532 || !id.switchUser {
		t.Fatalf("numeric: %+v %v", id, err)
	}
}

// TestDeployAsOtherUser runs a code deploy as root with run_as nobody: release files, links and commands
// belong to nobody, the archive owner is not kept.
func TestDeployAsOtherUser(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	nobody, err := user.Lookup("nobody")
	if err != nil {
		t.Skip("no user nobody")
	}
	env := newTestEnv(t)
	// t.TempDir parents are 0700 root: let nobody through
	for p := env.root; p != "/" && p != os.TempDir(); p = filepath.Dir(p) {
		_ = os.Chmod(p, 0o755)
	}
	id, err := resolveIdentity("nobody")
	if err != nil {
		t.Fatal(err)
	}
	env.deployer.run = newRunner(id, nil)
	env.deployer.cfg.SharedLinks = []SharedTarget{{Source: "/shared/media", Target: "pub/media"}}
	marker := filepath.Join(env.root, "whoami")
	writeFile(t, marker, "", 0o666)
	_ = os.Chmod(marker, 0o666)
	env.deployer.cfg.Deploy.LeaderCommands = []string{"id -u > " + shellQuote(marker)}

	if err := env.deploy("release_v1.tar.gz", map[string]string{"index.php": "v1"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(readFile(t, marker)); got != nobody.Uid {
		t.Fatalf("command ran as uid %s, want %s", got, nobody.Uid)
	}
	release := env.current()
	for _, p := range []string{env.currentLink(), release, filepath.Join(release, "index.php"), filepath.Join(release, "pub"), filepath.Join(release, "pub", "media"), env.releasesDir()} {
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		if uid := fmt.Sprint(fi.Sys().(*syscall.Stat_t).Uid); uid != nobody.Uid {
			t.Errorf("%s owned by %s, want %s", p, uid, nobody.Uid)
		}
	}
}

type recordingDeployer struct {
	calls []DeployPayload
}

func (r *recordingDeployer) Deploy(_ context.Context, p DeployPayload) error {
	r.calls = append(r.calls, p)
	return nil
}
