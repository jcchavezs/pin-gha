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
	dryRun                bool
	exitCode              bool
	includeActions        bool
	majorVersionAlignment bool
	allVersionAlignment   bool
}

func init() {
	rootCmd.PersistentFlags().Var(
		enumflag.New(&loglevel, "string", LevelIds, enumflag.EnumCaseInsensitive),
		"log-level",
		"Sets the log level",
	)

	rootCmd.AddCommand(repositoryCmd, ownerCmd, actionCmd, localRepositoryCmd)

	localRepositoryCmd.Flags().BoolVar(&patchFlags.exitCode, "exit-code", false, "Exit with code 2 if any workflow file was changed (files are still updated in place)")

	addVersionAlignmentFlags(localRepositoryCmd)

	localRepositoryCmd.Flags().BoolVar(&patchFlags.includeActions, "include-actions", false, "Also pin the actions used in action definitions (action.yml/action.yaml in the root and .github/actions/*)")
	repositoryCmd.Flags().BoolVar(&patchFlags.includeActions, "include-actions", false, "Also pin the actions used in action definitions (action.yml/action.yaml in the root and .github/actions/*)")
	ownerCmd.Flags().BoolVar(&patchFlags.includeActions, "include-actions", false, "Also pin the actions used in action definitions (action.yml/action.yaml in the root and .github/actions/*)")

	repositoryCmd.Flags().StringVar(&patchFlags.prBranch, "pr-branch", "pin-gh-actions", "Branch name used when creating or updating PRs")
	repositoryCmd.Flags().StringVar(&patchFlags.prBodyPath, "pr-body-path", "", "Path to a file whose content is used as the PR body (defaults to the built-in template)")
	repositoryCmd.Flags().StringSliceVar(&patchFlags.prTrustedOrgs, "trusted-org", nil, "Comma-separated list of GitHub organisations whose actions are left untouched")
	repositoryCmd.Flags().StringVar(&patchFlags.prCommitMsg, "pr-commit-msg", "chore(security): uses pinned versions of actions", "Commit message used when committing the pinned actions")
	repositoryCmd.Flags().BoolVar(&patchFlags.prFork, "fork", false, "Whether to fork the repository when creating the PR")
	repositoryCmd.Flags().BoolVar(&patchFlags.dryRun, "dry-run", false, "Show the changes that would be made without pushing or creating PRs")
	addVersionAlignmentFlags(repositoryCmd)

	ownerCmd.Flags().StringVar(&patchFlags.prBranch, "pr-branch", "pin-gh-actions", "Branch name used when creating or updating PRs")
	ownerCmd.Flags().StringVar(&patchFlags.prBodyPath, "pr-body-path", "", "Path to a file whose content is used as the PR body (defaults to the built-in template)")
	ownerCmd.Flags().StringSliceVar(&patchFlags.prTrustedOrgs, "trusted-org", nil, "Comma-separated list of GitHub organisations whose actions are left untouched")
	ownerCmd.Flags().StringVar(&patchFlags.prCommitMsg, "pr-commit-msg", "chore(security): uses pinned versions of actions", "Commit message used when committing the pinned actions")
	ownerCmd.Flags().BoolVar(&patchFlags.prFork, "fork", false, "Whether to fork the repository when creating the PR")
	ownerCmd.Flags().BoolVar(&patchFlags.dryRun, "dry-run", false, "Show the changes that would be made without pushing or creating PRs")
	addVersionAlignmentFlags(ownerCmd)
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
