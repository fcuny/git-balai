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

It never deletes the primary branch, the current branch, or a branch checked
out in another worktree. Each deleted branch is logged with its commit, so it
can be restored with `git branch <name> <commit>`.

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
