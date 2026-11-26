package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	iteratorexec "github.com/jcchavezs/gh-iterator/exec"

	"github.com/jcchavezs/pin-gha/patch"
	"github.com/spf13/cobra"
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

func getPRBodyFromPath(path string) (string, error) {
	var prBody string
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("reading PR body file: %w", err)
		}
		prBody = string(b)
	}

	return prBody, nil
}

var repositoryCmd = &cobra.Command{
	Use:   "repository [<name>]",
	Short: "Pin actions in a single GitHub repository",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cmd.SilenceUsage = true

		prBody, err := getPRBodyFromPath(patchFlags.prBodyPath)
		if err != nil {
			return fmt.Errorf("getting PR body: %w", err)
		}

		return patch.Repository(cmd.Context(), args[0], patch.PatchOptions{
			TargetBranch:             patchFlags.prBranch,
			PRBody:                   prBody,
			TrustedOrgs:              patchFlags.prTrustedOrgs,
			CommitMsg:                patchFlags.prCommitMsg,
			NeedsFork:                patchFlags.prFork,
			DryRun:                   patchFlags.dryRun,
			IncludeActionDefinitions: patchFlags.includeActions,
			OnDryRunChanges: func(ctx context.Context, repo string, d patch.DryRunDetails) {
				cmd.Printf("Dry run: repository %q would be updated (branch %q):\n%s\n", repo, d.Branch, d.Diff)
			},
			OnRepositorySkipped: func(ctx context.Context, repo string, reason patch.SkippedReason) {
				cmd.Printf("Repository %q skipped: %s\n", repo, reason.String())
			},
			OnPRCreated: func(ctx context.Context, repo string, p patch.PRDetails) {
				cmd.Printf("PR created for repository %q: %s\n", repo, p.URL)
			},
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
			TargetBranch:             patchFlags.prBranch,
			PRBody:                   prBody,
			TrustedOrgs:              patchFlags.prTrustedOrgs,
			CommitMsg:                patchFlags.prCommitMsg,
			NeedsFork:                patchFlags.prFork,
			DryRun:                   patchFlags.dryRun,
			IncludeActionDefinitions: patchFlags.includeActions,
			OnDryRunChanges: func(ctx context.Context, repo string, d patch.DryRunDetails) {
				cmd.Printf("Dry run: repository %q would be updated (branch %q):\n%s\n", repo, d.Branch, d.Diff)
			},
			OnRepositoryErr: func(ctx context.Context, repo string, err error) error {
				cmd.Printf("Error processing repository %q: %v\n", repo, err)
				return nil
			},
			OnRepositorySkipped: func(ctx context.Context, repo string, reason patch.SkippedReason) {
				cmd.Printf("Repository %q skipped: %s\n", repo, reason.String())
			},
			OnPRCreated: func(ctx context.Context, repo string, p patch.PRDetails) {
				cmd.Printf("PR created for repository %q: %s\n", repo, p.URL)
			},
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

var localRepositoryCmd = &cobra.Command{
	Use:   "local-repository [<name>|<path>]",
	Short: "Pin actions in a local repository",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cmd.SilenceUsage = true

		var changed []string
		err := patch.LocalRepository(cmd.Context(), args[0], patch.LocalPatchOptions{
			IncludeActionDefinitions: patchFlags.includeActions,
			OnFileChanged: func(_ context.Context, file string) {
				changed = append(changed, file)
				cmd.Printf("Pinned actions in %s\n", file)
			},
		})
		if err != nil {
			return err
		}

		if patchFlags.exitCode && len(changed) > 0 {
			return &changesError{files: changed}
		}

		return nil
	},
}

// exitCodeChanges is the exit code used by --exit-code when workflow files were
// changed. Errors exit with 1 so callers can tell the two apart.
const exitCodeChanges = 2

// changesError reports that workflow files were changed and --exit-code was set.
type changesError struct {
	files []string
}

func (e *changesError) Error() string {
	return fmt.Sprintf("%d workflow file(s) changed", len(e.files))
}

// stderrSuffix returns the stderr of the failed command, if any, to be appended to the error message.
func stderrSuffix(err error) string {
	if stderr, found := iteratorexec.StderrNotEmpty(iteratorexec.GetStderr(err)); found {
		return "\n" + strings.TrimRight(stderr, "\n")
	}
	return ""
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		var cerr *changesError
		if errors.As(err, &cerr) {
			rootCmd.PrintErrf("%v\n", cerr)
			os.Exit(exitCodeChanges)
		}

		if stderr, found := iteratorexec.StderrNotEmpty(iteratorexec.GetStderr(err)); found {
			rootCmd.PrintErr(stderr)
		}

		rootCmd.PrintErrf("Failed to execute command: %v\n", err)
		os.Exit(1)
	}
}
