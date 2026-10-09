// git-balai deletes local branches whose changes have already landed in the
// remote's primary branch, whether they were merged, rebased or squashed.
package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"
)

// version is set at build time with -ldflags "-X main.version=...". When
// empty, it is derived from the build information embedded by the Go toolchain.
var version string

func getVersion() string {
	if version != "" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	var revision, dirty string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "-dirty"
			}
		}
	}
	if revision != "" {
		return "devel-" + revision[:min(len(revision), 12)] + dirty
	}
	return "devel"
}

var preferredRemotes = []string{"origin", "github", "work"}

var symrefHeadRe = regexp.MustCompile(`ref: refs/heads/(\S+)\s+HEAD`)

type GitRepository struct {
	WorkingDirectory string
	RemoteName       string
	RemoteURL        string
	PrimaryBranch    string
}

// mergeKind describes how a branch's changes made it into the primary branch.
type mergeKind string

const (
	kindMerged   mergeKind = "merged"
	kindRebased  mergeKind = "rebased"
	kindSquashed mergeKind = "squashed"
)

type branch struct {
	Name   string
	SHA    string // abbreviated, for logs
	Commit string // full object name
}

type mergedBranch struct {
	branch
	Kind mergeKind
	// Worktree is the linked worktree the branch is checked out in, if any.
	// It has to be removed before the branch can be deleted.
	Worktree *worktree
}

type worktree struct {
	Path   string
	Branch string // empty when HEAD is detached
	Locked bool
	// Prunable is set when the worktree's directory is gone and only git's
	// administrative files remain.
	Prunable bool
}

func NewGitRepository(workingDir string) (*GitRepository, error) {
	repo := &GitRepository{
		WorkingDirectory: workingDir,
	}

	if err := repo.isGitRepository(); err != nil {
		return nil, err
	}

	if err := repo.workdirIsClean(); err != nil {
		return nil, err
	}

	if err := repo.guessRemote(); err != nil {
		return nil, err
	}

	if err := repo.guessPrimaryBranch(); err != nil {
		return nil, err
	}

	if err := repo.fetchPrimaryBranch(); err != nil {
		return nil, err
	}

	return repo, nil
}

// runGit runs git in the repository, feeding it stdin if not nil. On failure
// the error includes what git printed on stderr.
func (r *GitRepository) runGit(stdin io.Reader, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", r.WorkingDirectory}, args...)...)
	cmd.Stdin = stdin
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, msg)
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}

func (r *GitRepository) runGitCommand(args ...string) (string, error) {
	return r.runGit(nil, args...)
}

func (r *GitRepository) isGitRepository() error {
	if _, err := r.runGitCommand("rev-parse", "--show-toplevel"); err != nil {
		return fmt.Errorf("%s is not a git repository", r.WorkingDirectory)
	}
	return nil
}

func (r *GitRepository) workdirIsClean() error {
	out, err := r.runGitCommand("status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) != "" {
		return fmt.Errorf("%s has uncommitted changes, commit or stash them first", r.WorkingDirectory)
	}
	return nil
}

func (r *GitRepository) guessRemote() error {
	out, err := r.runGitCommand("remote")
	if err != nil {
		return err
	}

	remotes := strings.Fields(out)
	name := pickRemote(remotes)
	if name == "" {
		if len(remotes) == 0 {
			return errors.New("no remote configured")
		}
		return fmt.Errorf("none of the preferred remotes (%s) is configured, found: %s",
			strings.Join(preferredRemotes, ", "), strings.Join(remotes, ", "))
	}

	url, err := r.runGitCommand("remote", "get-url", name)
	if err != nil {
		return err
	}

	r.RemoteName = name
	r.RemoteURL = strings.TrimSpace(url)
	return nil
}

// pickRemote returns the first preferred remote that exists, or the only
// remote if there is exactly one. It returns "" when it can't decide.
func pickRemote(remotes []string) string {
	for _, preferred := range preferredRemotes {
		if slices.Contains(remotes, preferred) {
			return preferred
		}
	}
	if len(remotes) == 1 {
		return remotes[0]
	}
	return ""
}

func (r *GitRepository) guessPrimaryBranch() error {
	out, err := r.runGitCommand("ls-remote", "--symref", r.RemoteName, "HEAD")
	if err != nil {
		return err
	}

	matches := symrefHeadRe.FindStringSubmatch(out)
	if len(matches) < 2 {
		return errors.New("could not determine primary branch")
	}

	r.PrimaryBranch = matches[1]
	return nil
}

// upstreamRef is the remote-tracking ref for the primary branch. Branches are
// compared against it rather than the local primary branch, which may be stale.
func (r *GitRepository) upstreamRef() string {
	return "refs/remotes/" + r.RemoteName + "/" + r.PrimaryBranch
}

func (r *GitRepository) fetchPrimaryBranch() error {
	refspec := "+refs/heads/" + r.PrimaryBranch + ":" + r.upstreamRef()
	_, err := r.runGitCommand("fetch", "--quiet", r.RemoteName, refspec)
	return err
}

// localBranches returns all local branches with their abbreviated commit.
func (r *GitRepository) localBranches() ([]branch, error) {
	out, err := r.runGitCommand("for-each-ref", "--format=%(refname) %(objectname) %(objectname:short)", "refs/heads/")
	if err != nil {
		return nil, err
	}

	var branches []branch
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 3 {
			continue
		}
		branches = append(branches, branch{
			Name:   strings.TrimPrefix(fields[0], "refs/heads/"),
			Commit: fields[1],
			SHA:    fields[2],
		})
	}
	return branches, scanner.Err()
}

// remoteBranches asks the remote for its branches and returns the commit each
// one points to. The remote-tracking refs aren't used: they may be stale.
func (r *GitRepository) remoteBranches() (map[string]string, error) {
	out, err := r.runGitCommand("ls-remote", "--heads", r.RemoteName)
	if err != nil {
		return nil, err
	}
	return parseLsRemote(out), nil
}

// parseLsRemote parses the output of `git ls-remote --heads`.
func parseLsRemote(out string) map[string]string {
	heads := make(map[string]string)
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		commit, ref, ok := strings.Cut(scanner.Text(), "\t")
		if !ok || !strings.HasPrefix(ref, "refs/heads/") {
			continue
		}
		heads[strings.TrimPrefix(ref, "refs/heads/")] = commit
	}
	return heads
}

// ancestorBranches returns the local branches that are ancestors of the
// upstream primary branch.
func (r *GitRepository) ancestorBranches() (map[string]bool, error) {
	out, err := r.runGitCommand("for-each-ref", "--merged="+r.upstreamRef(), "--format=%(refname)", "refs/heads/")
	if err != nil {
		return nil, err
	}

	ancestors := make(map[string]bool)
	for _, ref := range strings.Fields(out) {
		ancestors[strings.TrimPrefix(ref, "refs/heads/")] = true
	}
	return ancestors, nil
}

// worktrees returns all the worktrees of the repository, the main one first.
func (r *GitRepository) worktrees() ([]worktree, error) {
	out, err := r.runGitCommand("worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	return parseWorktrees(out), nil
}

// parseWorktrees parses the output of `git worktree list --porcelain`.
func parseWorktrees(out string) []worktree {
	var wts []worktree
	for _, block := range strings.Split(strings.TrimSpace(out), "\n\n") {
		var wt worktree
		for _, line := range strings.Split(block, "\n") {
			key, value, _ := strings.Cut(line, " ")
			switch key {
			case "worktree":
				wt.Path = value
			case "branch":
				wt.Branch = strings.TrimPrefix(value, "refs/heads/")
			case "locked":
				wt.Locked = true
			case "prunable":
				wt.Prunable = true
			}
		}
		if wt.Path != "" {
			wts = append(wts, wt)
		}
	}
	return wts
}

// protectedBranches returns the branches that must not be deleted: the
// primary branch and the branches checked out in the main and the current
// worktree.
func (r *GitRepository) protectedBranches(wts []worktree) (map[string]bool, error) {
	current, err := r.runGitCommand("branch", "--show-current")
	if err != nil {
		return nil, err
	}

	protected := map[string]bool{r.PrimaryBranch: true}
	if current = strings.TrimSpace(current); current != "" {
		protected[current] = true
	}
	if len(wts) > 0 && wts[0].Branch != "" {
		protected[wts[0].Branch] = true
	}
	return protected, nil
}

// worktreeIsClean reports whether the worktree has no modified or untracked
// files, which `git worktree remove` would refuse to discard.
func (r *GitRepository) worktreeIsClean(path string) (bool, error) {
	out, err := r.runGitCommand("-C", path, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "", nil
}

// isRebased reports whether every commit on the branch has an equivalent
// commit (same patch) in the upstream primary branch.
func (r *GitRepository) isRebased(name string) (bool, error) {
	out, err := r.runGitCommand("cherry", r.upstreamRef(), "refs/heads/"+name)
	if err != nil {
		return false, err
	}

	lines := strings.Split(strings.TrimSpace(out), "\n")
	if lines[0] == "" {
		return false, nil
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, "- ") {
			return false, nil
		}
	}
	return true, nil
}

// isSquashed reports whether the branch's combined changes since it forked
// from the upstream primary branch match a single upstream commit.
func (r *GitRepository) isSquashed(name string) (bool, error) {
	ref := "refs/heads/" + name

	mergeBase, err := r.runGitCommand("merge-base", r.upstreamRef(), ref)
	if err != nil {
		return false, err
	}
	mergeBase = strings.TrimSpace(mergeBase)

	diff, err := r.runGitCommand("diff", "--no-color", "--no-ext-diff", mergeBase, ref)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(diff) == "" {
		return false, nil
	}

	branchIDs, err := r.patchIDs(diff)
	if err != nil || len(branchIDs) == 0 {
		return false, err
	}

	upstreamLog, err := r.runGitCommand("log", "-p", "--no-merges", "--no-color", "--no-ext-diff",
		mergeBase+".."+r.upstreamRef())
	if err != nil {
		return false, err
	}
	upstreamIDs, err := r.patchIDs(upstreamLog)
	if err != nil {
		return false, err
	}

	return slices.Contains(upstreamIDs, branchIDs[0]), nil
}

// patchIDs returns the stable patch ID of each patch in the input.
func (r *GitRepository) patchIDs(patches string) ([]string, error) {
	out, err := r.runGit(strings.NewReader(patches), "patch-id", "--stable")
	if err != nil {
		return nil, err
	}

	var ids []string
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		if id, _, ok := strings.Cut(scanner.Text(), " "); ok {
			ids = append(ids, id)
		}
	}
	return ids, scanner.Err()
}

// branchMergeKind reports how the branch landed in the upstream primary
// branch, or false if its changes aren't all there.
func (r *GitRepository) branchMergeKind(name string, ancestors map[string]bool) (mergeKind, bool, error) {
	if ancestors[name] {
		return kindMerged, true, nil
	}

	rebased, err := r.isRebased(name)
	if err != nil {
		return "", false, err
	}
	if rebased {
		return kindRebased, true, nil
	}

	squashed, err := r.isSquashed(name)
	if err != nil {
		return "", false, err
	}
	if squashed {
		return kindSquashed, true, nil
	}

	return "", false, nil
}

// getMergedBranches returns the local branches that can be safely deleted
// because their changes are in the upstream primary branch. A branch checked
// out in a linked worktree is only returned when removeWorktrees is set and
// the worktree is neither locked nor dirty.
func (r *GitRepository) getMergedBranches(removeWorktrees bool) ([]mergedBranch, error) {
	branches, err := r.localBranches()
	if err != nil {
		return nil, err
	}

	ancestors, err := r.ancestorBranches()
	if err != nil {
		return nil, err
	}

	wts, err := r.worktrees()
	if err != nil {
		return nil, err
	}

	protected, err := r.protectedBranches(wts)
	if err != nil {
		return nil, err
	}

	worktreeOf := make(map[string]*worktree)
	for i := range wts {
		if wts[i].Branch != "" {
			worktreeOf[wts[i].Branch] = &wts[i]
		}
	}

	var merged []mergedBranch
	for _, b := range branches {
		if b.Name == r.PrimaryBranch {
			continue
		}

		kind, ok, err := r.branchMergeKind(b.Name, ancestors)
		if err != nil {
			return nil, fmt.Errorf("checking branch %s: %w", b.Name, err)
		}
		if !ok {
			continue
		}

		if protected[b.Name] {
			log.Printf("Keeping %s: %s but checked out in the current or main worktree", b.Name, kind)
			continue
		}

		wt := worktreeOf[b.Name]
		if wt != nil && !wt.Prunable {
			if wt.Locked {
				log.Printf("Keeping %s: %s but checked out in locked worktree %s", b.Name, kind, wt.Path)
				continue
			}
			if !removeWorktrees {
				log.Printf("Keeping %s: %s but checked out in worktree %s (use -worktrees to remove it)", b.Name, kind, wt.Path)
				continue
			}
			clean, err := r.worktreeIsClean(wt.Path)
			if err != nil {
				return nil, fmt.Errorf("checking worktree %s: %w", wt.Path, err)
			}
			if !clean {
				log.Printf("Keeping %s: %s but worktree %s has changes", b.Name, kind, wt.Path)
				continue
			}
		}

		merged = append(merged, mergedBranch{branch: b, Kind: kind, Worktree: wt})
	}

	return merged, nil
}

// removeWorktree removes the worktree the branch is checked out in. A
// prunable worktree's directory is already gone, so pruning drops what's left.
// Removal isn't forced: git refuses if the worktree gained changes since
// getMergedBranches checked it.
func (r *GitRepository) removeWorktree(wt *worktree) error {
	args := []string{"worktree", "remove", wt.Path}
	if wt.Prunable {
		args = []string{"worktree", "prune"}
	}
	if _, err := r.runGitCommand(args...); err != nil {
		return fmt.Errorf("failed to remove worktree %s: %w", wt.Path, err)
	}
	return nil
}

// deleteBranch force-deletes a branch. `git branch -d` isn't enough: it checks
// against HEAD or the branch's own upstream rather than the remote primary
// branch, and it refuses squashed or rebased branches, which are never
// ancestors. getMergedBranches has already verified the branch is safe to drop.
func (r *GitRepository) deleteBranch(b mergedBranch) error {
	if _, err := r.runGitCommand("branch", "-D", b.Name); err != nil {
		return fmt.Errorf("failed to delete branch %s: %w", b.Name, err)
	}
	return nil
}

// deleteRemoteBranch deletes the branch on the remote. The lease makes the
// push fail if the remote branch moved since remoteBranches looked at it.
// A successful push also drops the remote-tracking ref.
func (r *GitRepository) deleteRemoteBranch(b mergedBranch) error {
	lease := "--force-with-lease=refs/heads/" + b.Name + ":" + b.Commit
	if _, err := r.runGitCommand("push", "--quiet", lease, "--delete", r.RemoteName, "refs/heads/"+b.Name); err != nil {
		return fmt.Errorf("failed to delete remote branch %s/%s: %w", r.RemoteName, b.Name, err)
	}
	return nil
}

type cleanupOptions struct {
	DryRun bool
	// Worktrees removes the linked worktrees merged branches are checked out in.
	Worktrees bool
	// Remote also deletes the merged branches on the remote.
	Remote bool
}

// cleanupMergedBranches deletes all local branches that have been merged into the primary branch
func (r *GitRepository) cleanupMergedBranches(opts cleanupOptions) error {
	mergedBranches, err := r.getMergedBranches(opts.Worktrees)
	if err != nil {
		return err
	}

	if len(mergedBranches) == 0 {
		log.Println("No merged branches to clean up")
		return nil
	}

	// A remote branch is only deleted when it points to the same commit as
	// the local one. Otherwise it has commits that weren't checked, or it is
	// an unrelated branch with the same name.
	var remoteHeads map[string]string
	if opts.Remote {
		if remoteHeads, err = r.remoteBranches(); err != nil {
			return err
		}
	}
	deleteRemote := func(b mergedBranch) bool {
		commit, ok := remoteHeads[b.Name]
		if ok && commit != b.Commit {
			log.Printf("Keeping %s/%s: it points to %.7s, not %s", r.RemoteName, b.Name, commit, b.SHA)
		}
		return ok && commit == b.Commit
	}

	log.Printf("Found %d merged branches:", len(mergedBranches))
	var errs []error
	for _, b := range mergedBranches {
		if opts.DryRun {
			if b.Worktree != nil {
				log.Printf("Would remove worktree: %s", b.Worktree.Path)
			}
			log.Printf("Would delete: %s (%s, was %s)", b.Name, b.Kind, b.SHA)
			if deleteRemote(b) {
				log.Printf("Would delete remote branch: %s/%s", r.RemoteName, b.Name)
			}
			continue
		}
		if b.Worktree != nil {
			if err := r.removeWorktree(b.Worktree); err != nil {
				log.Printf("Error: %v", err)
				errs = append(errs, err)
				continue
			}
			log.Printf("Removed worktree: %s", b.Worktree.Path)
		}
		if err := r.deleteBranch(b); err != nil {
			log.Printf("Error: %v", err)
			errs = append(errs, err)
			continue
		}
		log.Printf("Deleted branch: %s (%s, was %s)", b.Name, b.Kind, b.SHA)
		if deleteRemote(b) {
			if err := r.deleteRemoteBranch(b); err != nil {
				log.Printf("Error: %v", err)
				errs = append(errs, err)
				continue
			}
			log.Printf("Deleted remote branch: %s/%s", r.RemoteName, b.Name)
		}
	}

	return errors.Join(errs...)
}

func main() {
	dryRun := flag.Bool("dry-run", false, "show which branches would be deleted without deleting them")
	removeWorktrees := flag.Bool("worktrees", false, "also remove clean, unlocked worktrees of merged branches")
	deleteRemote := flag.Bool("remote", false, "also delete merged branches on the remote, if they point to the same commit")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: git-balai [-dry-run] [-worktrees] [-remote] [-version]\n\n"+
			"Delete local branches that have been merged, rebased or squashed into the remote's primary branch.\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() > 0 {
		flag.Usage()
		os.Exit(2)
	}
	if *showVersion {
		fmt.Println("git-balai", getVersion())
		return
	}

	wd, err := os.Getwd()
	if err != nil {
		log.Fatalf("Failed to get working directory: %v", err)
	}

	repo, err := NewGitRepository(wd)
	if err != nil {
		log.Fatalf("Error: %v", err)
	}

	log.Printf("Working in: %s", repo.WorkingDirectory)
	log.Printf("Main branch is: %s", repo.PrimaryBranch)
	log.Printf("Remote is named %s and is at %s", repo.RemoteName, repo.RemoteURL)

	if *dryRun {
		log.Println("Running in dry-run mode")
	}

	opts := cleanupOptions{DryRun: *dryRun, Worktrees: *removeWorktrees, Remote: *deleteRemote}
	if err := repo.cleanupMergedBranches(opts); err != nil {
		log.Fatalf("Failed to cleanup merged branches: %v", err)
	}
}
