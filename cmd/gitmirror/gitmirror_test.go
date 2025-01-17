// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/build/internal/envutil"
	repospkg "golang.org/x/build/repos"
)

func TestHomepage(t *testing.T) {
	tm := newTestMirror(t)
	if body := tm.get("/"); !strings.Contains(body, "build") {
		t.Errorf("expected body to contain \"build\", didn't: %q", body)
	}
}

func TestDebugWatcher(t *testing.T) {
	tm := newTestMirror(t)
	tm.commit("hello world")
	tm.loopOnce()

	body := tm.get("/debug/watcher/build")
	if substr := `watcher status for repo: "build"`; !strings.Contains(body, substr) {
		t.Fatalf("GET /debug/watcher/build: want %q in body, got %s", substr, body)
	}
	if substr := "waiting"; !strings.Contains(body, substr) {
		t.Fatalf("GET /debug/watcher/build: want %q in body, got %s", substr, body)
	}
}

func TestArchive(t *testing.T) {
	tm := newTestMirror(t)

	// Start with a revision we know about.
	tm.commit("hello world")
	initialRev := strings.TrimSpace(tm.git(tm.gerrit, "rev-parse", "HEAD"))
	tm.loopOnce() // fetch the commit.
	tm.get("/build.tar.gz?rev=" + initialRev)

	// Now test one we don't see yet. It will be fetched automatically.
	tm.commit("round two")
	secondRev := strings.TrimSpace(tm.git(tm.gerrit, "rev-parse", "HEAD"))
	// As of writing, the git version installed on the builders has some kind
	// of bug that prevents the "git fetch" this triggers from working. Skip.
	if strings.HasPrefix(tm.git(tm.gerrit, "version"), "git version 2.11") {
		t.Skip("known-buggy git version")
	}
	tm.get("/build.tar.gz?rev=" + secondRev)

	// Pick it up normally and re-fetch it to make sure we don't get confused.
	tm.loopOnce()
	tm.get("/build.tar.gz?rev=" + secondRev)
}

func TestMirror(t *testing.T) {
	tm := newTestMirror(t)
	for i := 0; i < 2; i++ {
		tm.commit(fmt.Sprintf("revision %v", i))
		rev := tm.git(tm.gerrit, "rev-parse", "HEAD")
		tm.loopOnce()
		if githubRev := tm.git(tm.github, "rev-parse", "HEAD"); rev != githubRev {
			t.Errorf("github HEAD is %v, want %v", githubRev, rev)
		}
		if csrRev := tm.git(tm.csr, "rev-parse", "HEAD"); rev != csrRev {
			t.Errorf("csr HEAD is %v, want %v", csrRev, rev)
		}
	}
}

// Tests that mirroring an initially empty repository works. See golang/go#39597.
// The repository still has to exist.
func TestMirrorInitiallyEmpty(t *testing.T) {
	tm := newTestMirror(t)
	if err := tm.m.repos["build"].loopOnce(); err == nil {
		t.Error("expected error mirroring empty repository, got none")
	}
	tm.commit("first commit")
	tm.loopOnce()
	rev := tm.git(tm.gerrit, "rev-parse", "HEAD")
	if githubRev := tm.git(tm.github, "rev-parse", "HEAD"); rev != githubRev {
		t.Errorf("github HEAD is %v, want %v", githubRev, rev)
	}
}

type testMirror struct {
	// Local paths to the copies of the build repo.
	gerrit, github, csr string
	m                   *gitMirror
	server              *httptest.Server
	buildRepo           *repo
	t                   *testing.T
}

// newTestMirror returns a mirror configured to watch the "build" repository
// and mirror it to GitHub and CSR. All repositories are faked out with local
// versions created hermetically. The mirror is idle and must be pumped with
// loopOnce.
func newTestMirror(t *testing.T) *testMirror {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("skipping; git not in PATH")
	}

	goBase := t.TempDir()
	gerrit := filepath.Join(goBase, "build")
	if err := os.Mkdir(gerrit, 0755); err != nil {
		t.Fatalf("error creating gerrit build directory: %v", err)
	}

	tm := &testMirror{
		gerrit: gerrit,
		github: t.TempDir(),
		csr:    t.TempDir(),
		m: &gitMirror{
			mux:      http.NewServeMux(),
			cacheDir: t.TempDir(),
			homeDir:  t.TempDir(),
			// gitMirror generally expects goBase to be a URL, not
			// a path, but git handles local paths just fine. As a
			// result, gitMirror uses standard string concatenation
			// rather than path.Join. Ensure the path ends in / to
			// make sure concatenation is OK.
			goBase:       goBase + "/",
			repos:        map[string]*repo{},
			mirrorGitHub: true,
			mirrorCSR:    true,
			timeoutScale: 0,
		},
		t: t,
	}
	tm.m.mux.HandleFunc("/", tm.m.handleRoot)
	tm.server = httptest.NewServer(tm.m.mux)
	t.Cleanup(tm.server.Close)

	// The origin is non-bare so we can commit to it; the destinations are
	// bare so we can push to them.
	initRepo := func(dir string, bare bool) {
		initArgs := []string{"init"}
		if bare {
			initArgs = append(initArgs, "--bare")
		}
		for _, args := range [][]string{
			initArgs,
			{"config", "user.name", "Gopher"},
			{"config", "user.email", "gopher@golang.org"},
		} {
			cmd := exec.Command("git", args...)
			envutil.SetDir(cmd, dir)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%s: %v\n%s", strings.Join(cmd.Args, " "), err, out)
			}
		}
	}
	initRepo(tm.gerrit, false)
	initRepo(tm.github, true)
	initRepo(tm.csr, true)

	tm.buildRepo = tm.m.addRepo(&repospkg.Repo{
		GoGerritProject:    "build",
		ImportPath:         "golang.org/x/build",
		MirrorToGitHub:     true,
		GitHubRepo:         "golang/build",
		MirrorToCSRProject: "golang-org",
	})
	if err := tm.buildRepo.init(); err != nil {
		t.Fatal(err)
	}

	// Manually add mirror repos. We can't use tm.m.addMirrors, as they
	// hard-codes the real remotes, but we need to use local test
	// directories.
	tm.buildRepo.addRemote("github", tm.github, "")
	tm.buildRepo.addRemote("csr", tm.csr, "")

	return tm
}

func (tm *testMirror) loopOnce() {
	tm.t.Helper()
	if err := tm.buildRepo.loopOnce(); err != nil {
		tm.t.Fatal(err)
	}
}

func (tm *testMirror) commit(content string) {
	tm.t.Helper()
	if err := ioutil.WriteFile(filepath.Join(tm.gerrit, "README"), []byte(content), 0777); err != nil {
		tm.t.Fatal(err)
	}
	tm.git(tm.gerrit, "add", ".")
	tm.git(tm.gerrit, "commit", "-m", content)
}

func (tm *testMirror) git(dir string, args ...string) string {
	tm.t.Helper()
	cmd := exec.Command("git", args...)
	envutil.SetDir(cmd, dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		tm.t.Fatalf("git: %v, %s", err, out)
	}
	return string(out)
}

func (tm *testMirror) get(path string) string {
	tm.t.Helper()
	resp, err := http.Get(tm.server.URL + path)
	if err != nil {
		tm.t.Fatal(err)
	}
	body, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		tm.t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		tm.t.Fatalf("request for %q failed", path)
	}
	return string(body)
}
