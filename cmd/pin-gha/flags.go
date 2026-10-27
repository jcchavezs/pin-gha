package main

import (
	"github.com/jcchavezs/pin-gha/patch"
	"github.com/spf13/cobra"
	"github.com/thediveo/enumflag/v2"
)

var patchFlags struct {
	prBranch              string
	prTitle               string
	prBodyPath            string
	prCommitMsg           string
	prTrustedOrgs         []string
	prFork                bool
	majorVersionAlignment bool
	allVersionAlignment   bool
}

func init() {
	rootCmd.PersistentFlags().Var(
		enumflag.New(&loglevel, "string", LevelIds, enumflag.EnumCaseInsensitive),
		"log-level",
		"Sets the log level",
	)

	rootCmd.AddCommand(repositoryCmd, organizationCmd, actionCmd, localRepositoryCmd)

	localRepositoryCmd.Flags().StringVar(&patchFlags.prBranch, "pr-branch", "pin-gh-actions", "Branch name used when creating or updating PRs")
	localRepositoryCmd.Flags().StringVar(&patchFlags.prBodyPath, "pr-body-path", "", "Path to a file whose content is used as the PR body (defaults to the built-in template)")
	localRepositoryCmd.Flags().StringArrayVar(&patchFlags.prTrustedOrgs, "trusted-org", nil, "List of GitHub organisations whose actions are left untouched")
	localRepositoryCmd.Flags().StringVar(&patchFlags.prCommitMsg, "pr-commit-msg", "chore(security): uses pinned versions of actions", "Commit message used when committing the pinned actions")
	addVersionAlignmentFlags(localRepositoryCmd)

	repositoryCmd.Flags().StringVar(&patchFlags.prBranch, "pr-branch", "pin-gh-actions", "Branch name used when creating or updating PRs")
	repositoryCmd.Flags().StringVar(&patchFlags.prBodyPath, "pr-body-path", "", "Path to a file whose content is used as the PR body (defaults to the built-in template)")
	repositoryCmd.Flags().StringArrayVar(&patchFlags.prTrustedOrgs, "trusted-org", nil, "List of GitHub organisations whose actions are left untouched")
	repositoryCmd.Flags().StringVar(&patchFlags.prCommitMsg, "pr-commit-msg", "chore(security): uses pinned versions of actions", "Commit message used when committing the pinned actions")
	repositoryCmd.Flags().BoolVar(&patchFlags.prFork, "fork", false, "Whether to fork the repository when creating the PR")
	addVersionAlignmentFlags(repositoryCmd)

	organizationCmd.Flags().StringVar(&patchFlags.prBranch, "pr-branch", "pin-gh-actions", "Branch name used when creating or updating PRs")
	organizationCmd.Flags().StringVar(&patchFlags.prBodyPath, "pr-body-path", "", "Path to a file whose content is used as the PR body (defaults to the built-in template)")
	organizationCmd.Flags().StringArrayVar(&patchFlags.prTrustedOrgs, "trusted-org", nil, "List of GitHub organisations whose actions are left untouched")
	organizationCmd.Flags().StringVar(&patchFlags.prCommitMsg, "pr-commit-msg", "chore(security): uses pinned versions of actions", "Commit message used when committing the pinned actions")
	organizationCmd.Flags().BoolVar(&patchFlags.prFork, "fork", false, "Whether to fork the repository when creating the PR")
	addVersionAlignmentFlags(organizationCmd)
}

func addVersionAlignmentFlags(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&patchFlags.majorVersionAlignment, "major-version-alignment", false, "Align uses of the same action within each major version")
	cmd.Flags().BoolVar(&patchFlags.allVersionAlignment, "all-version-alignment", false, "Align all uses of the same action to one version")
	cmd.MarkFlagsMutuallyExclusive("major-version-alignment", "all-version-alignment")
}

func versionAlignment() patch.VersionAlignment {
	switch {
	case patchFlags.majorVersionAlignment:
		return patch.VersionAlignmentMajor
	case patchFlags.allVersionAlignment:
		return patch.VersionAlignmentAll
	default:
		return patch.VersionAlignmentNone
	}
}
