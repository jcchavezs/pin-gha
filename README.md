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

```sh
pin-gha repository <owner/repo>
```

### Pin all repositories in an organization

```sh
pin-gha organization <org-name>
```

### Pin a local repository

Modifies workflow files in place without creating a PR.

```sh
pin-gha local-repository <path>
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

The `repository` and `organization` subcommands share the following flags:

| Flag             | Default                                            | Description                                                                        |
| ---------------- | -------------------------------------------------- | ---------------------------------------------------------------------------------- |
| `--pr-branch`    | `pin-actions`                                      | Branch name used when creating or updating PRs                                     |
| `--pr-body-path` | _(built-in template)_                              | Path to a file whose content is used as the PR body                                |
| `--trusted-orgs` | `atko-cic`                                         | Comma-separated list of GitHub organisations whose actions are left untouched      |
| `--pr-commit-msg`| `chore(security): uses pinned versions of actions` | Commit message used when committing pinned actions                                 |

Global flags:

| Flag          | Default  | Description                                          |
| ------------- | -------- | ---------------------------------------------------- |
| `--log-level` | _(info)_ | Sets the log level (`debug`, `info`, `warn`, `error`)|

## Behaviour

- Actions already pinned to a full commit hash (40 hex characters) are left untouched.
- Actions from trusted organisations are skipped.
- Local actions (prefixed with `./`) are skipped.
- Actions pinned to a branch (`main`, `master`) or a non-full semver tag (`v1`, `v1.2`) get a `# TODO: use a release instead` comment in addition to the hash.
- When a version cannot be resolved, the original reference is preserved with a TODO comment.
