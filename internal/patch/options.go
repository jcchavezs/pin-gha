package patch

import (
	"fmt"
	"os"
)

const defaultTagetBranch = "pin-actions"
const defaultCommitMsg = "chore(security): uses pinned versions of actions"

// PatchOptions controls the behaviour of the patching process.
type PatchOptions struct {
	// TargetBranch is the branch name used when creating/updating PRs.
	// Defaults to "pin-actions" when empty.
	TargetBranch string
	// PRBodyPath is an optional path to a file whose content is used as the PR
	// body. When empty the embedded pr-body.md is used.
	PRBodyPath string
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

	return o
}

func (o PatchOptions) prBodyContent() (string, error) {
	if o.PRBodyPath != "" {
		b, err := os.ReadFile(o.PRBodyPath)
		if err != nil {
			return "", fmt.Errorf("reading PR body file: %w", err)
		}
		return string(b), nil
	}
	return "", nil
}
