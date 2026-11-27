# pin-gha 🇧🇷

A CLI tool and a library that pins GitHub Actions workflow steps to specific commit hashes, improving supply chain security by preventing tag mutation attacks.

![pinga](pinga.png)

Before:

```yaml
- uses: actions/checkout@v4
```

After:

```yaml
- uses: actions/checkout@11bd71901bbe5b1630ceea73d27597364c9af683 # v4.2.2
```

## Prerequisites

- [`git`](https://git-scm.com/)
- [`gh`](https://cli.github.com/) (authenticated)

## Installation

```sh
go install github.com/jcchavezs/pin-gha/cmd/pin-gha@latest
```

## Usage

### Pin a single remote repository

Clones the repository, pins all unpinned actions, and opens a PR with the changes.
The repository can be specified as `<owner>/<repo>` or as a full GitHub URL.

```sh
pin-gha repository <owner/repo>
```

### Pin repositories of an owner (user or organization)

For user accounts, only public repositories are processed, even when authenticated. Use `pin-gha repository <owner/repo>` for private user repositories.

```sh
pin-gha owner <owner>
```

### Pin a local repository

Modifies workflow files in place without creating a PR.

```sh
pin-gha local-repository <path>
```

Pass `--exit-code` to make the command exit with code `2` when it changed any workflow file (the files are still updated in place). It exits with `0` when everything is already pinned and with `1` on errors, which makes it usable in CI:

```sh
pin-gha local-repository --exit-code . || { echo "Workflow actions are not pinned"; exit 1; }
```

### Pin action definitions (opt-in)

By default only `.github/workflows` is processed. Pass `--include-actions` (available in `local-repository`, `repository` and `owner`) to also pin the `uses` of composite action steps in `action.yml`/`action.yaml` files, both in the repository root and in `.github/actions/*/`.

```sh
pin-gha local-repository --include-actions .
```

### Resolve a single action

Prints the commit hash for a specific action and version without modifying any files.

```sh
pin-gha action <owner/repo> <version>
```

Example:

```sh
$ pin-gha action actions/checkout v4
Proposed version: v4
Resolved version: v4.2.2
Hash: 11bd71901bbe5b1630ceea73d27597364c9af683
```

## Options

The `repository` and `owner` subcommands share the following flags:

| Flag             | Default                                            | Description                                                                        |
| ---------------- | -------------------------------------------------- | ---------------------------------------------------------------------------------- |
| `--pr-branch`    | `pin-gh-actions`                                   | Branch name used when creating or updating PRs                                     |
| `--pr-body-path` | _(built-in template)_                              | Path to a file whose content is used as the PR body                                |
| `--trusted-org`  |                                                    | Comma-separated list of GitHub organisations whose actions are left untouched      |
| `--dry-run`      | `false`                                            | Print the changes that would be made without pushing, forking or creating PRs      |
| `--fork`         | `false`                                            | Fork the repository and open the PR from the fork                                  |
| `--include-actions` | `false`                                         | Also pin `uses` in `action.yml`/`action.yaml` action definitions                   |
| `--pr-commit-msg`| `chore(security): uses pinned versions of actions` | Commit message used when committing pinned actions                                 |

The `repository`, `owner` and `local-repository` subcommands also support version alignment, which pins the uses of the same action found in a repository to a single version:

| Flag                        | Description                                                                                       |
| --------------------------- | ------------------------------------------------------------------------------------------------- |
| `--major-version-alignment` | Aligns the uses of each action within the same major version                                      |
| `--all-version-alignment`   | Aligns all the uses of each action to one version, whatever their major (exclusive with the above) |

```sh
pin-gha repository owner/repo --major-version-alignment
# Or align across all major versions:
pin-gha repository owner/repo --all-version-alignment
```

Alignment is applied per repository, so with `owner` each repository is aligned on its own. The actions of the same repository (e.g. `github/codeql-action/init` and `github/codeql-action/analyze`) are aligned together. Each use is pinned to the highest resolved release of its group (`v4.2.0` over `v4.1.0`). Pre-releases are never chosen as the target and a use is never moved backwards. References that are not releases, such as branches, and uses already pinned to a commit hash are left as they are.

Global flags:

| Flag          | Default  | Description                                          |
| ------------- | -------- | ---------------------------------------------------- |
| `--log-level` | _(info)_ | Sets the log level (`debug`, `info`, `warn`, `error`)|

## Behaviour

- Actions already pinned to a full commit hash (40 hex characters) are left untouched.
- Actions from trusted organisations are skipped.
- Reusable workflows called with a job-level `uses:` (e.g. `org/repo/.github/workflows/build.yml@v1`) are pinned like any other action.
- Local actions (prefixed with `./`) are skipped.
- Docker image actions (prefixed with `docker://`) are skipped, see [#29](https://github.com/jcchavezs/pin-gha/issues/29).
- A floating tag (`v1`, `v1.2`) is pinned to the commit it points to today and commented with the release tag at that commit (e.g. `# v1.100.0`). If the tag does not exist, the highest release tag starting with `v1.` is used.
- Actions pinned to a branch (`main`, `master`) or to a floating tag without a release tag at its commit get a `# TODO: use a release instead` comment in addition to the hash.
- When a version cannot be resolved, the original reference is preserved with a TODO comment.
