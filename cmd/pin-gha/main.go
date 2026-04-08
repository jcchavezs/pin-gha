package main

import (
	"fmt"
	"log/slog"
	"os"

	iteratorexec "github.com/jcchavezs/gh-iterator/exec"

	"github.com/jcchavezs/pin-gha/patch"
	"github.com/spf13/cobra"
	"github.com/thediveo/enumflag/v2"
)

var rootCmd = &cobra.Command{
	Use:   "pin-gha",
	Short: "Check and pin GitHub Actions to specific commit hashes",
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		h := slog.NewTextHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{
			Level: loglevel,
		})
		slog.SetDefault(slog.New(h))
		return nil
	},
	SilenceUsage:  false,
	SilenceErrors: true,
}

var patchFlags struct {
	prBranch      string
	prTitle       string
	prBodyPath    string
	prCommitMsg   string
	prTrustedOrgs []string
}

func getPRBodyFromPath(path string) (string, error) {
	var prBody string
	if patchFlags.prBodyPath != "" {
		b, err := os.ReadFile(patchFlags.prBodyPath)
		if err != nil {
			return "", fmt.Errorf("reading PR body file: %w", err)
		}
		prBody = string(b)
	}

	return prBody, nil
}

var repositoryCmd = &cobra.Command{
	Use:   "repository [<name>|<path>]",
	Short: "Pin actions in a single GitHub repository or a local repository",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cmd.SilenceUsage = true

		prBody, err := getPRBodyFromPath(patchFlags.prBodyPath)
		if err != nil {
			return fmt.Errorf("getting PR body: %w", err)
		}

		return patch.Repository(cmd.Context(), args[0], patch.PatchOptions{
			TargetBranch: patchFlags.prBranch,
			PRBody:       prBody,
			TrustedOrgs:  patchFlags.prTrustedOrgs,
			CommitMsg:    patchFlags.prCommitMsg,
		})
	},
}

var organizationCmd = &cobra.Command{
	Use:   "organization <name>",
	Short: "Pin actions across all repositories in a GitHub organization",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cmd.SilenceUsage = true

		prBody, err := getPRBodyFromPath(patchFlags.prBodyPath)
		if err != nil {
			return fmt.Errorf("getting PR body: %w", err)
		}

		return patch.Organization(cmd.Context(), args[0], patch.PatchOptions{
			TargetBranch: patchFlags.prBranch,
			PRBody:       prBody,
			TrustedOrgs:  patchFlags.prTrustedOrgs,
			CommitMsg:    patchFlags.prCommitMsg,
		})
	},
}

var actionCmd = &cobra.Command{
	Use:   "action <name> <version>",
	Short: "Resolve the commit hash for a specific GitHub Action version",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		cmd.SilenceUsage = true
		hash, resolvedVersion, err := patch.ResolveCommitHashForAction(cmd.Context(), args[0], args[1])
		if err != nil {
			return err
		}

		cmd.Printf("Proposed version: %s\nResolved version: %s\nHash: %s\n", args[1], resolvedVersion, hash)
		return nil
	},
}

func init() {
	rootCmd.PersistentFlags().Var(
		enumflag.New(&loglevel, "string", LevelIds, enumflag.EnumCaseInsensitive),
		"log-level",
		"Sets the log level",
	)

	rootCmd.AddCommand(repositoryCmd, organizationCmd, actionCmd)

	repositoryCmd.Flags().StringVar(&patchFlags.prBranch, "pr-branch", "pin-actions", "Branch name used when creating or updating PRs")
	repositoryCmd.Flags().StringVar(&patchFlags.prBodyPath, "pr-body-path", "", "Path to a file whose content is used as the PR body (defaults to the built-in template)")
	repositoryCmd.Flags().StringSliceVar(&patchFlags.prTrustedOrgs, "trusted-orgs", []string{"atko-cic"}, "Comma-separated list of GitHub organisations whose actions are left untouched")
	repositoryCmd.Flags().StringVar(&patchFlags.prCommitMsg, "pr-commit-msg", "chore(security): uses pinned versions of actions", "Commit message used when committing the pinned actions")

	organizationCmd.Flags().StringVar(&patchFlags.prBranch, "pr-branch", "pin-actions", "Branch name used when creating or updating PRs")
	organizationCmd.Flags().StringVar(&patchFlags.prBodyPath, "pr-body-path", "", "Path to a file whose content is used as the PR body (defaults to the built-in template)")
	organizationCmd.Flags().StringSliceVar(&patchFlags.prTrustedOrgs, "trusted-orgs", []string{"atko-cic"}, "Comma-separated list of GitHub organisations whose actions are left untouched")
	organizationCmd.Flags().StringVar(&patchFlags.prCommitMsg, "pr-commit-msg", "chore(security): uses pinned versions of actions", "Commit message used when committing the pinned actions")
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		if stderr, found := iteratorexec.StderrNotEmpty(iteratorexec.GetStderr(err)); found {
			rootCmd.PrintErr(stderr)
		}

		rootCmd.PrintErrf("Failed to execute command: %v\n", err)
		os.Exit(1)
	}
}
