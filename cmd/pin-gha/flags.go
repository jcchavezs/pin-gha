package main

import (
	"github.com/thediveo/enumflag/v2"
)

var patchFlags struct {
	prBranch      string
	prTitle       string
	prBodyPath    string
	prCommitMsg   string
	prTrustedOrgs []string
	prFork        bool
}

func init() {
	rootCmd.PersistentFlags().Var(
		enumflag.New(&loglevel, "string", LevelIds, enumflag.EnumCaseInsensitive),
		"log-level",
		"Sets the log level",
	)

	rootCmd.AddCommand(repositoryCmd, organizationCmd, actionCmd, localRepositoryCmd)

	repositoryCmd.Flags().StringVar(&patchFlags.prBranch, "pr-branch", "pin-gh-actions", "Branch name used when creating or updating PRs")
	repositoryCmd.Flags().StringVar(&patchFlags.prBodyPath, "pr-body-path", "", "Path to a file whose content is used as the PR body (defaults to the built-in template)")
	repositoryCmd.Flags().StringSliceVar(&patchFlags.prTrustedOrgs, "trusted-orgs", []string{"atko-cic"}, "Comma-separated list of GitHub organisations whose actions are left untouched")
	repositoryCmd.Flags().StringVar(&patchFlags.prCommitMsg, "pr-commit-msg", "chore(security): uses pinned versions of actions", "Commit message used when committing the pinned actions")
	repositoryCmd.Flags().BoolVar(&patchFlags.prFork, "fork", false, "Whether to fork the repository when creating the PR")

	organizationCmd.Flags().StringVar(&patchFlags.prBranch, "pr-branch", "pin-gh-actions", "Branch name used when creating or updating PRs")
	organizationCmd.Flags().StringVar(&patchFlags.prBodyPath, "pr-body-path", "", "Path to a file whose content is used as the PR body (defaults to the built-in template)")
	organizationCmd.Flags().StringSliceVar(&patchFlags.prTrustedOrgs, "trusted-orgs", []string{"atko-cic"}, "Comma-separated list of GitHub organisations whose actions are left untouched")
	organizationCmd.Flags().StringVar(&patchFlags.prCommitMsg, "pr-commit-msg", "chore(security): uses pinned versions of actions", "Commit message used when committing the pinned actions")
	organizationCmd.Flags().BoolVar(&patchFlags.prFork, "fork", false, "Whether to fork the repository when creating the PR")
}