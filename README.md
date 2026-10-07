# git-balai

*balai* is French for broom. `git-balai` deletes local branches whose changes
have already landed in the remote's primary branch.

## Install

```sh
go install fcuny.net/git-balai@latest
```

With the binary on your `PATH`, it can also be run as `git balai`.

## Usage

```sh
git balai            # delete merged branches
git balai -dry-run   # only show what would be deleted
git balai -worktrees # also remove worktrees of merged branches
git balai -version   # print the version
```

It runs in the current repository and:

1. Refuses to run if there are staged or unstaged changes to tracked files.
2. Picks the remote: the first of `origin`, `github` or `work` that exists, or
   the only remote if there is just one.
3. Asks the remote which branch its `HEAD` points to (the primary branch) and
   fetches it, so a stale local primary branch doesn't hide merged branches.
4. Deletes every local branch whose changes are in the remote primary branch:
   - **merged**: the branch is an ancestor of the primary branch;
   - **rebased**: every commit has an equivalent commit (same patch) upstream;
   - **squashed**: the branch's combined changes match a single upstream commit.

It never deletes the primary branch, or the branch checked out in the current
or the main worktree. Each deleted branch is logged with its commit, so it can
be restored with `git branch <name> <commit>`.

A merged branch checked out in a linked worktree is kept, unless `-worktrees`
is given. Then the worktree is removed with `git worktree remove` and the
branch is deleted, but only if the worktree is not locked and has no modified
or untracked files. Ignored files (build output, `.env`, ...) don't count as
changes and are removed with the worktree, so check with `-dry-run` first.
A worktree whose directory was already deleted is pruned without `-worktrees`.

A squash merge whose conflicts were resolved differently from the branch, or
that was edited before landing, won't match and the branch is kept.

## Development

```sh
make          # lint, test and build into bin/
make test
make lint
```

## License

BSD 3-Clause, see [LICENSE](LICENSE).
