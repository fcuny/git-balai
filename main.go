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
	"slices"
	"strings"
)

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
	Name string
	SHA  string
}

type mergedBranch struct {
	branch
	Kind mergeKind
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
	out, err := r.runGitCommand("for-each-ref", "--format=%(refname) %(objectname:short)", "refs/heads/")
	if err != nil {
		return nil, err
	}

	var branches []branch
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		ref, sha, ok := strings.Cut(scanner.Text(), " ")
		if !ok {
			continue
		}
		branches = append(branches, branch{Name: strings.TrimPrefix(ref, "refs/heads/"), SHA: sha})
	}
	return branches, scanner.Err()
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

// protectedBranches returns the branches that must not be deleted: the
// primary branch and any branch checked out in a worktree, including the
// current one.
func (r *GitRepository) protectedBranches() (map[string]bool, error) {
	out, err := r.runGitCommand("worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}

	protected := map[string]bool{r.PrimaryBranch: true}
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		if name, ok := strings.CutPrefix(scanner.Text(), "branch refs/heads/"); ok {
			protected[name] = true
		}
	}
	return protected, scanner.Err()
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
// because their changes are in the upstream primary branch.
func (r *GitRepository) getMergedBranches() ([]mergedBranch, error) {
	branches, err := r.localBranches()
	if err != nil {
		return nil, err
	}

	ancestors, err := r.ancestorBranches()
	if err != nil {
		return nil, err
	}

	protected, err := r.protectedBranches()
	if err != nil {
		return nil, err
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
			log.Printf("Keeping %s: %s but checked out in a worktree", b.Name, kind)
			continue
		}

		merged = append(merged, mergedBranch{branch: b, Kind: kind})
	}

	return merged, nil
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

// cleanupMergedBranches deletes all local branches that have been merged into the primary branch
func (r *GitRepository) cleanupMergedBranches(dryRun bool) error {
	mergedBranches, err := r.getMergedBranches()
	if err != nil {
		return err
	}

	if len(mergedBranches) == 0 {
		log.Println("No merged branches to clean up")
		return nil
	}

	log.Printf("Found %d merged branches:", len(mergedBranches))
	var errs []error
	for _, b := range mergedBranches {
		if dryRun {
			log.Printf("Would delete: %s (%s, was %s)", b.Name, b.Kind, b.SHA)
			continue
		}
		if err := r.deleteBranch(b); err != nil {
			log.Printf("Error: %v", err)
			errs = append(errs, err)
			continue
		}
		log.Printf("Deleted branch: %s (%s, was %s)", b.Name, b.Kind, b.SHA)
	}

	return errors.Join(errs...)
}

func main() {
	dryRun := flag.Bool("dry-run", false, "show which branches would be deleted without deleting them")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: git-balai [-dry-run]\n\n"+
			"Delete local branches that have been merged, rebased or squashed into the remote's primary branch.\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() > 0 {
		flag.Usage()
		os.Exit(2)
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

	if err := repo.cleanupMergedBranches(*dryRun); err != nil {
		log.Fatalf("Failed to cleanup merged branches: %v", err)
	}
}
