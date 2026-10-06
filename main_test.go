package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// isolateGit makes git ignore the user's and the system's configuration.
func isolateGit(t *testing.T) {
	t.Helper()
	config := filepath.Join(t.TempDir(), "gitconfig")
	content := "[user]\n\tname = Test\n\temail = test@example.com\n[init]\n\tdefaultBranch = main\n"
	if err := os.WriteFile(config, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", config)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func commit(t *testing.T, dir, file, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", file)
	git(t, dir, "commit", "--quiet", "-m", "update "+file)
}

type fixture struct {
	root   string
	remote string // bare repository acting as the server
	work   string // the clone git-balai runs in
	other  string // another clone, used to change the remote behind work's back
}

func newFixture(t *testing.T, remoteName string) *fixture {
	t.Helper()
	isolateGit(t)

	root := t.TempDir()
	f := &fixture{
		root:   root,
		remote: filepath.Join(root, "remote.git"),
		work:   filepath.Join(root, "work"),
		other:  filepath.Join(root, "other"),
	}

	git(t, root, "init", "--quiet", "--bare", f.remote)
	git(t, root, "clone", "--quiet", "--origin", remoteName, f.remote, f.work)
	commit(t, f.work, "README", "hello\n")
	git(t, f.work, "push", "--quiet", remoteName, "main")
	git(t, root, "clone", "--quiet", f.remote, f.other)

	return f
}

// pushFeature creates a branch in work with the given files committed one by
// one, pushes it, and switches work back to main.
func (f *fixture) pushFeature(t *testing.T, remoteName, name string, files ...string) {
	t.Helper()
	git(t, f.work, "switch", "--quiet", "-c", name, "main")
	for _, file := range files {
		commit(t, f.work, file, name+" "+file+"\n")
	}
	git(t, f.work, "push", "--quiet", remoteName, name)
	git(t, f.work, "switch", "--quiet", "main")
}

// landOnRemote runs a sequence of git commands in the other clone, on an
// up-to-date main, and pushes the result.
func (f *fixture) landOnRemote(t *testing.T, cmds ...[]string) {
	t.Helper()
	git(t, f.other, "fetch", "--quiet", "origin")
	git(t, f.other, "switch", "--quiet", "main")
	git(t, f.other, "reset", "--quiet", "--hard", "origin/main")
	for _, args := range cmds {
		git(t, f.other, args...)
	}
	git(t, f.other, "push", "--quiet", "origin", "main")
}

func (f *fixture) branches(t *testing.T) []string {
	t.Helper()
	return strings.Fields(git(t, f.work, "for-each-ref", "--format=%(refname:short)", "refs/heads/"))
}

func runCleanup(t *testing.T, dir string, dryRun bool) []mergedBranch {
	t.Helper()
	repo, err := NewGitRepository(dir)
	if err != nil {
		t.Fatalf("NewGitRepository: %v", err)
	}
	merged, err := repo.getMergedBranches()
	if err != nil {
		t.Fatalf("getMergedBranches: %v", err)
	}
	if err := repo.cleanupMergedBranches(dryRun); err != nil {
		t.Fatalf("cleanupMergedBranches: %v", err)
	}
	return merged
}

func assertBranches(t *testing.T, f *fixture, want ...string) {
	t.Helper()
	got := f.branches(t)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("branches = %v, want %v", got, want)
	}
}

func kinds(merged []mergedBranch) map[string]mergeKind {
	m := make(map[string]mergeKind)
	for _, b := range merged {
		m[b.Name] = b.Kind
	}
	return m
}

func TestDeletesMergedBranches(t *testing.T) {
	f := newFixture(t, "origin")
	f.pushFeature(t, "origin", "merged", "a")
	f.pushFeature(t, "origin", "unmerged", "b")
	f.landOnRemote(t, []string{"merge", "--quiet", "--no-ff", "-m", "merge", "origin/merged"})

	// work's local main is now behind the remote: the branch must still be
	// detected as merged.
	merged := runCleanup(t, f.work, false)

	if got := kinds(merged); got["merged"] != kindMerged || len(got) != 1 {
		t.Errorf("merged branches = %v", got)
	}
	assertBranches(t, f, "main", "unmerged")
}

func TestDeletesSquashedBranch(t *testing.T) {
	f := newFixture(t, "origin")
	f.pushFeature(t, "origin", "squashed", "a", "b")
	f.landOnRemote(t,
		[]string{"commit", "--quiet", "--allow-empty", "-m", "unrelated"},
		[]string{"merge", "--quiet", "--squash", "origin/squashed"},
		[]string{"commit", "--quiet", "-m", "squashed"},
	)

	merged := runCleanup(t, f.work, false)

	if got := kinds(merged); got["squashed"] != kindSquashed {
		t.Errorf("merged branches = %v", got)
	}
	assertBranches(t, f, "main")
}

func TestDeletesRebasedBranch(t *testing.T) {
	f := newFixture(t, "origin")
	f.pushFeature(t, "origin", "rebased", "a", "b")
	f.landOnRemote(t,
		[]string{"commit", "--quiet", "--allow-empty", "-m", "unrelated"},
		[]string{"cherry-pick", "main..origin/rebased"},
	)

	merged := runCleanup(t, f.work, false)

	if got := kinds(merged); got["rebased"] != kindRebased {
		t.Errorf("merged branches = %v", got)
	}
	assertBranches(t, f, "main")
}

func TestKeepsPartiallyLandedBranch(t *testing.T) {
	f := newFixture(t, "origin")
	f.pushFeature(t, "origin", "partial", "a", "b")
	f.landOnRemote(t, []string{"cherry-pick", "origin/partial~1"})

	if merged := runCleanup(t, f.work, false); len(merged) != 0 {
		t.Errorf("merged branches = %v, want none", kinds(merged))
	}
	assertBranches(t, f, "main", "partial")
}

func TestKeepsCheckedOutBranches(t *testing.T) {
	f := newFixture(t, "origin")
	f.pushFeature(t, "origin", "current", "a")
	f.pushFeature(t, "origin", "in-worktree", "b")
	f.landOnRemote(t,
		[]string{"merge", "--quiet", "--no-ff", "-m", "merge", "origin/current"},
		[]string{"merge", "--quiet", "--no-ff", "-m", "merge", "origin/in-worktree"},
	)
	git(t, f.work, "switch", "--quiet", "current")
	git(t, f.work, "worktree", "add", "--quiet", filepath.Join(f.root, "wt"), "in-worktree")

	if merged := runCleanup(t, f.work, false); len(merged) != 0 {
		t.Errorf("merged branches = %v, want none", kinds(merged))
	}
	assertBranches(t, f, "main", "current", "in-worktree")
}

func TestDryRunDeletesNothing(t *testing.T) {
	f := newFixture(t, "origin")
	f.pushFeature(t, "origin", "merged", "a")
	f.landOnRemote(t, []string{"merge", "--quiet", "--no-ff", "-m", "merge", "origin/merged"})

	if merged := runCleanup(t, f.work, true); len(merged) != 1 {
		t.Errorf("merged branches = %v, want one", kinds(merged))
	}
	assertBranches(t, f, "main", "merged")
}

func TestSingleNonStandardRemote(t *testing.T) {
	f := newFixture(t, "my-fork_2")
	f.pushFeature(t, "my-fork_2", "merged", "a")
	f.landOnRemote(t, []string{"merge", "--quiet", "--no-ff", "-m", "merge", "origin/merged"})

	runCleanup(t, f.work, false)

	assertBranches(t, f, "main")
}

func TestRejectsUncommittedChanges(t *testing.T) {
	f := newFixture(t, "origin")
	readme := filepath.Join(f.work, "README")

	if err := os.WriteFile(filepath.Join(f.work, "untracked"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewGitRepository(f.work); err != nil {
		t.Errorf("untracked files should be allowed, got %v", err)
	}

	if err := os.WriteFile(readme, []byte("unstaged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewGitRepository(f.work); err == nil {
		t.Error("expected an error for unstaged changes")
	}

	git(t, f.work, "add", "README")
	if _, err := NewGitRepository(f.work); err == nil {
		t.Error("expected an error for staged changes")
	}
}

func TestGitErrorsIncludeStderr(t *testing.T) {
	isolateGit(t)
	repo := &GitRepository{WorkingDirectory: t.TempDir()}
	_, err := repo.runGitCommand("rev-parse", "HEAD")
	if err == nil || !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("error = %v, want git's stderr in it", err)
	}
}

func TestPickRemote(t *testing.T) {
	tests := []struct {
		remotes []string
		want    string
	}{
		{[]string{"upstream", "origin"}, "origin"},
		{[]string{"work", "github"}, "github"},
		{[]string{"my-fork"}, "my-fork"},
		{[]string{"a", "b"}, ""},
		{nil, ""},
	}
	for _, tt := range tests {
		if got := pickRemote(tt.remotes); got != tt.want {
			t.Errorf("pickRemote(%v) = %q, want %q", tt.remotes, got, tt.want)
		}
	}
}
