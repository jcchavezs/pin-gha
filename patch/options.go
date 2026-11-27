package patch

import (
	"context"
	"log/slog"
)

const defaultTagetBranch = "pin-gha"

const defaultCommitMsg = "chore(sec|gha): uses pinned versions of github actions"

type PRDetails struct {
	URL string
}

// DryRunDetails describes the changes that would have been pushed in a dry run.
type DryRunDetails struct {
	// Branch is the branch that would have been pushed.
	Branch string
	// Diff is the staged diff of the workflow files.
	Diff string
	// Files are the workflow files that would have changed.
	Files []string
}

type SkippedReason int

const (
	SkippedReasonUnknown SkippedReason = iota
	SkippedReasonEmptyRepository
	SkippedReasonAlreadyPinned
	SkippedReasonNoWorkflows
)

func (r SkippedReason) String() string {
	switch r {
	case SkippedReasonEmptyRepository:
		return "empty repository"
	case SkippedReasonAlreadyPinned:
		return "already pinned actions"
	case SkippedReasonNoWorkflows:
		return "no workflows found"
	default:
		return "unknown reason"
	}
}

// PatchOptions controls the behaviour of the patching process.
type PatchOptions struct {
	// TargetBranch is the branch name used when creating/updating PRs.
	// Defaults to "pin-actions" when empty.
	TargetBranch string
	// PRBody is the content of the PR body.
	PRBody string
	// PRAsDraft indicates whether the created PR should be a draft.
	PRAsDraft bool
	// DryRun runs the pinning logic on the clone but skips fork, push and PR creation.
	DryRun bool
	// NeedsFork indicates whether to fork the repository when creating the PR.
	NeedsFork bool
	// VersionAlignment selects how the versions of the same action are aligned
	// across the workflows of a repository.
	VersionAlignment VersionAlignment
	// TrustedOrgs is the list of GitHub organisations whose actions are left
	// untouched.
	TrustedOrgs []string
	// IncludeActionDefinitions also pins the actions used in action definition
	// files (action.yml/action.yaml in the repository root and in
	// .github/actions/*). Disabled by default.
	IncludeActionDefinitions bool
	// CommitMsg is the commit message used when committing the pinned actions.
	// Defaults to "chore(security): uses pinned versions of actions" when empty.
	CommitMsg string
	// LogHandler is a function that retrieves a slog.Handler from the context.
	LogHandler slog.Handler

	// OnRepositoryErr is a callback that will be called when an error occurs while processing a repository.
	// The error returned by this callback will be considered the final error for that repository, and it will be returned by the patching functions.
	// If this callback is not set, any error will be returned as-is.
	OnRepositoryErr func(context.Context, string, error) error

	// OnRepositorySkipped is a callback that will be called when a repository is skipped, with the reason for skipping.
	OnRepositorySkipped func(context.Context, string, SkippedReason)

	// OnPRCreated is a callback that will be called when a PR is created.
	OnPRCreated func(context.Context, string, PRDetails)

	// OnDryRunChanges is called, only in dry-run mode, for each repository that would have been changed.
	OnDryRunChanges func(context.Context, string, DryRunDetails)
}

const defaultPRBody = `This pull request updates the GitHub Actions workflow files to use pinned commit SHAs for all third-party actions, improving security and reproducibility.`

func (o PatchOptions) withDefaults() PatchOptions {
	if o.TargetBranch == "" {
		o.TargetBranch = defaultTagetBranch
	}
	if o.CommitMsg == "" {
		o.CommitMsg = defaultCommitMsg
	}

	if o.PRBody == "" {
		o.PRBody = defaultPRBody
	}

	if o.LogHandler == nil {
		o.LogHandler = slog.Default().Handler()
	}

	if o.OnRepositoryErr == nil {
		o.OnRepositoryErr = func(_ context.Context, _ string, err error) error { return err }
	}

	if o.OnRepositorySkipped == nil {
		o.OnRepositorySkipped = func(context.Context, string, SkippedReason) {}
	}

	if o.OnPRCreated == nil {
		o.OnPRCreated = func(context.Context, string, PRDetails) {}
	}

	if o.OnDryRunChanges == nil {
		o.OnDryRunChanges = func(context.Context, string, DryRunDetails) {}
	}

	return o
}

type LocalPatchOptions struct {
	// TrustedOrgs is the list of GitHub organisations whose actions are left
	// untouched.
	TrustedOrgs []string
	// VersionAlignment selects how the versions of the same action are aligned
	// across the workflows of the repository.
	VersionAlignment VersionAlignment

	// IncludeActionDefinitions also pins the actions used in action definition
	// files (action.yml/action.yaml in the repository root and in
	// .github/actions/*). Disabled by default.
	IncludeActionDefinitions bool

	// LogHandler is a function that retrieves a slog.Handler from the context.
	LogHandler slog.Handler

	// OnFileChanged is called with the path (relative to the repository root)
	// of each workflow file that was modified.
	OnFileChanged func(context.Context, string)
}

func (lo LocalPatchOptions) withDefaults() LocalPatchOptions {
	if lo.LogHandler == nil {
		lo.LogHandler = slog.Default().Handler()
	}

	if lo.OnFileChanged == nil {
		lo.OnFileChanged = func(context.Context, string) {}
	}

	return lo
}
