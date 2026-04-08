package patch

const defaultTagetBranch = "pin-actions"
const defaultCommitMsg = "chore(security): uses pinned versions of actions"

// PatchOptions controls the behaviour of the patching process.
type PatchOptions struct {
	// TargetBranch is the branch name used when creating/updating PRs.
	// Defaults to "pin-actions" when empty.
	TargetBranch string
	// PRBody is the content of the PR body.
	PRBody string
	// TrustedOrgs is the list of GitHub organisations whose actions are left
	// untouched.
	TrustedOrgs []string
	// CommitMsg is the commit message used when committing the pinned actions.
	// Defaults to "chore(security): uses pinned versions of actions" when empty.
	CommitMsg string
}

func (o PatchOptions) withDefaults() PatchOptions {
	if o.TargetBranch == "" {
		o.TargetBranch = defaultTagetBranch
	}
	if o.CommitMsg == "" {
		o.CommitMsg = defaultCommitMsg
	}

	if o.PRBody == "" {
		o.PRBody = o.CommitMsg
	}

	return o
}
