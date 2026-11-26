package patch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	iterator "github.com/jcchavezs/gh-iterator"
	iteratorexec "github.com/jcchavezs/gh-iterator/exec"
	"github.com/jcchavezs/gh-iterator/exec/mock"
	"github.com/spf13/afero"
	"github.com/stretchr/testify/require"
)

func TestCheckCommandExist(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("PATH", tempDir)

	t.Run("Existing command", func(t *testing.T) {
		fakeCmdPath := filepath.Join(tempDir, "thisIsFakeCmd")
		require.NoError(t, os.WriteFile(fakeCmdPath, []byte("#!/bin/sh\necho 'fake command'"), 0755))

		require.NoError(t, checkCommandExist("thisIsFakeCmd"))
	})

	t.Run("Non-existing command", func(t *testing.T) {
		err := checkCommandExist("thisDoesNotExist")
		require.ErrorContains(t, err, "thisDoesNotExist")
	})
}

func TestMissingCommands(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	t.Run("Owner returns error", func(t *testing.T) {
		err := Owner(context.Background(), "org", PatchOptions{})
		require.ErrorContains(t, err, "failed to find")
	})

	t.Run("Repository returns error", func(t *testing.T) {
		err := Repository(context.Background(), "org/repo", PatchOptions{})
		require.ErrorContains(t, err, "failed to find")
	})

	t.Run("LocalRepository does not need git or gh", func(t *testing.T) {
		require.NoError(t, LocalRepository(context.Background(), t.TempDir(), LocalPatchOptions{}))
	})
}

// workflowMemFS returns an afero.MemMapFs with a workflow that has nothing to pin,
// so patchFS finds files to process but makes no changes.
func workflowMemFS() afero.Fs {
	fs := afero.NewMemMapFs()
	_ = fs.MkdirAll(".github/workflows", 0755)
	_ = afero.WriteFile(fs, ".github/workflows/ci.yml", []byte("jobs: {}\n"), 0644)
	return fs
}

var nonEmptyRepository = iterator.Repository{Name: "org/repo", Size: 1}

func TestPatchRepository_EmptyRepository(t *testing.T) {
	var skippedReason SkippedReason
	opts := PatchOptions{
		OnRepositorySkipped: func(_ context.Context, _ string, reason SkippedReason) { skippedReason = reason },
	}.withDefaults()

	// A mock execer without functions fails the test if any command is run.
	require.NoError(t, patchRepository(t.Context(), slog.New(slog.DiscardHandler), iterator.Repository{Name: "org/repo"}, mock.Execer{}, opts))
	require.Equal(t, SkippedReasonEmptyRepository, skippedReason)
}

func TestPatchRepository_NoWorkflows(t *testing.T) {
	var skippedReason SkippedReason
	opts := PatchOptions{
		OnRepositorySkipped: func(_ context.Context, _ string, reason SkippedReason) { skippedReason = reason },
	}.withDefaults()

	// A mock execer without functions fails the test if any command is run.
	xr := mock.Execer{GenerateFSFn: func() afero.Fs { return afero.NewMemMapFs() }}
	require.NoError(t, patchRepository(t.Context(), slog.New(slog.DiscardHandler), nonEmptyRepository, xr, opts))
	require.Equal(t, SkippedReasonNoWorkflows, skippedReason)
	require.Equal(t, "no workflows found", skippedReason.String())
}

func TestResolveCommitHashInRepository_EmptyRepository(t *testing.T) {
	_, _, err := resolveCommitHashInRepository(t.Context(), iterator.Repository{Name: "org/action"}, mock.Execer{}, "v4")
	require.ErrorIs(t, err, errUnresolvedVersion)
}

// TestPatchRepository_NoChanges verifies that when HasChanges returns false
// (git status -s produces empty output), patchRepository returns nil without
// attempting to create a branch or PR.
func TestPatchRepository_NoChanges(t *testing.T) {
	ctx := context.Background()

	xr := mock.Execer{
		// GenerateFS returns a workflow with nothing to pin, so no patches are applied.
		GenerateFSFn: func() afero.Fs {
			return workflowMemFS()
		},
		// HasChanges calls Run("git", "status", "-s") and expects empty stdout → no changes.
		RunFn: func(ctx context.Context, command string, args ...string) (iteratorexec.Result, error) {
			if command == "git" && len(args) > 0 && args[0] == "status" {
				return iteratorexec.Result{Stdout: "", ExitCode: 0}, nil
			}
			return iteratorexec.Result{}, fmt.Errorf("unexpected Run call: %s %v", command, args)
		},
		// RunX should never be called — if it is, the test fails.
		RunXFn: func(ctx context.Context, command string, args ...string) (string, error) {
			t.Errorf("unexpected RunX call: %s %v", command, args)
			return "", nil
		},
	}

	require.NoError(t, patchRepository(ctx, slog.New(slog.DiscardHandler), nonEmptyRepository, xr, PatchOptions{}.withDefaults()))
}

func TestNormalizeRepositoryName(t *testing.T) {
	tests := []struct {
		name string
		repo string
		want string
	}{
		{name: "owner and name", repo: "org/repo", want: "org/repo"},
		{name: "GitHub URL", repo: "https://github.com/org/repo", want: "org/repo"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, normalizeRepositoryName(tt.repo))
		})
	}
}

// TestPatchRepository_HasChanges_NewPR verifies the happy path: when HasChanges
// returns true and no existing open PR exists, patchRepository creates the branch,
// stages changes, commits, pushes, and opens a new PR.
func TestPatchRepository_HasChanges_NewPR(t *testing.T) {
	ctx := context.Background()

	var (
		checkedOutBranch string
		addedPath        string
		commitMessage    string
		pushedBranch     string
	)

	xr := mock.Execer{
		GenerateFSFn: func() afero.Fs {
			return workflowMemFS()
		},
		RunFn: func(ctx context.Context, command string, args ...string) (iteratorexec.Result, error) {
			switch {
			case command == "git" && len(args) > 0 && args[0] == "status":
				// HasChanges: return non-empty stdout to signal changes exist.
				return iteratorexec.Result{Stdout: "M  .github/workflows/ci.yml\n", ExitCode: 0}, nil
			case command == "gh" && len(args) > 0 && args[0] == "pr" && args[1] == "view":
				// CreatePRIfNotExist: pr view returns exit code 1 → no existing PR.
				return iteratorexec.Result{ExitCode: 1}, nil
			default:
				return iteratorexec.Result{}, fmt.Errorf("unexpected Run call: %s %v", command, args)
			}
		},
		RunXFn: func(ctx context.Context, command string, args ...string) (string, error) {
			switch {
			case command == "git" && len(args) >= 2 && args[0] == "checkout" && args[1] == "-b":
				checkedOutBranch = args[2]
				return "", nil
			case command == "git" && len(args) >= 1 && args[0] == "add":
				addedPath = args[1]
				return "", nil
			case command == "git" && len(args) >= 2 && args[0] == "commit" && args[1] == "-m":
				commitMessage = args[2]
				return "", nil
			case command == "git" && len(args) >= 1 && args[0] == "push":
				// push --force origin <branch>
				pushedBranch = args[len(args)-1]
				return "", nil
			case command == "gh" && len(args) >= 2 && args[0] == "pr" && args[1] == "create":
				return "https://github.com/org/repo/pull/1", nil
			default:
				return "", fmt.Errorf("unexpected RunX call: %s %v", command, args)
			}
		},
	}

	require.NoError(t, patchRepository(ctx, slog.New(slog.DiscardHandler), nonEmptyRepository, xr, PatchOptions{}.withDefaults()))
	require.Equal(t, defaultTagetBranch, checkedOutBranch)
	require.Equal(t, ".github/workflows", addedPath)
	require.Equal(t, "chore(sec|gha): uses pinned versions of github actions", commitMessage)
	require.Equal(t, defaultTagetBranch, pushedBranch)
}

// TestPatchRepository_IncludeActions_OnlyAddsExistingPaths verifies that paths missing in
// the repository are not staged, since git add fails on them.
func TestPatchRepository_IncludeActions_OnlyAddsExistingPaths(t *testing.T) {
	var added []string
	xr := mock.Execer{
		GenerateFSFn: func() afero.Fs {
			fs := afero.NewMemMapFs()
			require.NoError(t, fs.MkdirAll(".github/workflows", 0755))
			require.NoError(t, afero.WriteFile(fs, "action.yml", []byte("name: x\n"), 0644))
			return fs
		},
		RunFn: func(ctx context.Context, command string, args ...string) (iteratorexec.Result, error) {
			if command == "git" && args[0] == "status" {
				return iteratorexec.Result{Stdout: "M  action.yml\n"}, nil
			}
			return iteratorexec.Result{}, fmt.Errorf("unexpected Run call: %s %v", command, args)
		},
		RunXFn: func(ctx context.Context, command string, args ...string) (string, error) {
			switch {
			case command == "git" && args[0] == "add":
				added = append(added, args[1])
				return "", nil
			case command == "git" && args[0] == "checkout":
				return "", nil
			case mock.CallIs(t, command, args, "git", "diff", "--cached", "--name-only"), mock.CallIs(t, command, args, "git", "diff", "--cached"):
				return "", nil
			default:
				return "", fmt.Errorf("unexpected RunX call: %s %v", command, args)
			}
		},
	}

	opts := PatchOptions{DryRun: true, IncludeActionDefinitions: true}.withDefaults()
	require.NoError(t, patchRepository(t.Context(), slog.New(slog.DiscardHandler), nonEmptyRepository, xr, opts))
	require.Equal(t, []string{".github/workflows", "action.yml"}, added)
}

// TestPatchRepository_HasChanges_PRAlreadyExists verifies that when an open PR
// already exists, patchRepository succeeds (updates the PR) without creating a new one.
func TestPatchRepository_HasChanges_PRAlreadyExists(t *testing.T) {
	ctx := context.Background()

	xr := mock.Execer{
		GenerateFSFn: func() afero.Fs {
			return workflowMemFS()
		},
		RunFn: func(ctx context.Context, command string, args ...string) (iteratorexec.Result, error) {
			switch {
			case command == "git" && len(args) > 0 && args[0] == "status":
				return iteratorexec.Result{Stdout: "M  .github/workflows/ci.yml\n", ExitCode: 0}, nil
			case command == "gh" && len(args) > 0 && args[0] == "pr" && args[1] == "view":
				// Existing open PR.
				return iteratorexec.Result{
					Stdout:   `{"url":"https://github.com/org/repo/pull/5","state":"OPEN","isDraft":false}`,
					ExitCode: 0,
				}, nil
			default:
				return iteratorexec.Result{}, fmt.Errorf("unexpected Run call: %s %v", command, args)
			}
		},
		RunXFn: func(ctx context.Context, command string, args ...string) (string, error) {
			switch {
			case command == "git" && len(args) >= 2 && args[0] == "checkout" && args[1] == "-b":
				return "", nil
			case command == "git" && len(args) >= 1 && args[0] == "add":
				return "", nil
			case command == "git" && len(args) >= 2 && args[0] == "commit" && args[1] == "-m":
				return "", nil
			case command == "git" && len(args) >= 1 && args[0] == "push":
				return "", nil
			case command == "gh" && len(args) >= 2 && args[0] == "pr" && (args[1] == "edit" || args[1] == "create"):
				return "https://github.com/org/repo/pull/5", nil
			default:
				return "", fmt.Errorf("unexpected RunX call: %s %v", command, args)
			}
		},
	}

	require.NoError(t, patchRepository(ctx, slog.New(slog.DiscardHandler), nonEmptyRepository, xr, PatchOptions{}.withDefaults()))
}

// TestPatchRepository_HasChangesError verifies that an error from HasChanges
// is propagated back to the caller.
func TestPatchRepository_HasChangesError(t *testing.T) {
	ctx := context.Background()

	xr := mock.Execer{
		GenerateFSFn: func() afero.Fs {
			return workflowMemFS()
		},
		RunFn: func(ctx context.Context, command string, args ...string) (iteratorexec.Result, error) {
			return iteratorexec.Result{}, errors.New("git status failed")
		},
	}

	require.Error(t, patchRepository(ctx, slog.New(slog.DiscardHandler), nonEmptyRepository, xr, PatchOptions{}.withDefaults()))
}

// ---- helpers for patchLocalRepositoryFS tests ----

// mockHash is a valid 40-character commit hash used across tests.
const mockHash = "abcdef1234567890abcdef1234567890abcdef12"

// setupWorkflow writes content to .github/workflows/ci.yml inside fs.
func setupWorkflow(t *testing.T, fs afero.Fs, content string) {
	t.Helper()
	require.NoError(t, fs.MkdirAll(".github/workflows", 0755), "creating workflows dir")
	require.NoError(t, afero.WriteFile(fs, ".github/workflows/ci.yml", []byte(content), 0644), "writing workflow file")
}

// readWorkflowContent reads .github/workflows/ci.yml from fs.
func readWorkflowContent(t *testing.T, fs afero.Fs) string {
	t.Helper()
	b, err := afero.ReadFile(fs, ".github/workflows/ci.yml")
	require.NoError(t, err, "reading workflow file")
	return string(b)
}

// withMockResolver replaces the resolveCommitHash package variable for the
// duration of the test and restores the original in t.Cleanup.
func withMockResolver(t *testing.T, fn func(_ context.Context, action, version string) (string, string, error)) {
	t.Helper()
	orig := resolveCommitHash
	resolveCommitHash = fn
	t.Cleanup(func() { resolveCommitHash = orig })
}

// patchLocalRepositoryFSErr runs patchLocalRepositoryFS discarding the changed files.
func patchLocalRepositoryFSErr(ctx context.Context, logger *slog.Logger, fs afero.Fs, trustedOrgs []string) error {
	_, err := patchLocalRepositoryFS(ctx, logger, fs, trustedOrgs)
	return err
}

func TestPatchLocalRepositoryFS(t *testing.T) {
	t.Run("no workflows directory returns nil", func(t *testing.T) {
		require.NoError(t, patchLocalRepositoryFSErr(t.Context(), slog.New(slog.DiscardHandler), afero.NewMemMapFs(), nil))
	})

	t.Run("non yaml files are ignored", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		require.NoError(t, fs.MkdirAll(".github/workflows", 0755))
		require.NoError(t, afero.WriteFile(fs, ".github/workflows/notes.txt", []byte("not yaml"), 0644))
		require.NoError(t, patchLocalRepositoryFSErr(t.Context(), slog.New(slog.DiscardHandler), fs, nil))
		// Verify the non-yaml file is unchanged.
		b, err := afero.ReadFile(fs, ".github/workflows/notes.txt")
		require.NoError(t, err)
		require.Equal(t, "not yaml", string(b))
	})

	t.Run("step with empty uses is skipped", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		const content = "jobs:\n  build:\n    steps:\n      - name: Run something\n"
		setupWorkflow(t, fs, content)
		require.NoError(t, patchLocalRepositoryFSErr(t.Context(), slog.New(slog.DiscardHandler), fs, nil))
		require.Equal(t, content, readWorkflowContent(t, fs))
	})

	t.Run("trusted org action is skipped", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		const content = "jobs:\n  build:\n    steps:\n      - uses: jcchavezs/my-action@v1.2.3\n"
		setupWorkflow(t, fs, content)
		require.NoError(t, patchLocalRepositoryFSErr(t.Context(), slog.New(slog.DiscardHandler), fs, []string{"jcchavezs"}))
		require.Equal(t, content, readWorkflowContent(t, fs))
	})

	t.Run("local action with existing path is skipped", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		const content = "jobs:\n  build:\n    steps:\n      - uses: ./local-action\n"
		setupWorkflow(t, fs, content)
		// afero.Exists normalises ./local-action → local-action; create the dir.
		require.NoError(t, fs.MkdirAll("local-action", 0755))
		require.NoError(t, patchLocalRepositoryFSErr(t.Context(), slog.New(slog.DiscardHandler), fs, nil))
		require.Equal(t, content, readWorkflowContent(t, fs))
	})

	t.Run("docker actions are skipped", func(t *testing.T) {
		for _, uses := range []string{
			"docker://alpine:3.8",
			"docker://ghcr.io/OWNER/IMAGE_NAME",
			"docker://gcr.io/cloud-builders/gradle",
			"docker://alpine@sha256:" + strings.Repeat("a", 64),
		} {
			fs := afero.NewMemMapFs()
			content := fmt.Sprintf("jobs:\n  build:\n    steps:\n      - uses: %s\n", uses)
			setupWorkflow(t, fs, content)
			require.NoError(t, patchLocalRepositoryFSErr(t.Context(), slog.New(slog.DiscardHandler), fs, nil), uses)
			require.Equal(t, content, readWorkflowContent(t, fs), uses)
		}
	})

	t.Run("already pinned 40-char hash is skipped", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		content := fmt.Sprintf("jobs:\n  build:\n    steps:\n      - uses: actions/checkout@%s\n", mockHash)
		setupWorkflow(t, fs, content)
		require.NoError(t, patchLocalRepositoryFSErr(t.Context(), slog.New(slog.DiscardHandler), fs, nil))
		require.Equal(t, content, readWorkflowContent(t, fs))
	})

	t.Run("missing version returns error", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		const content = "jobs:\n  build:\n    steps:\n      - uses: actions/checkout\n"
		setupWorkflow(t, fs, content)
		require.Error(t, patchLocalRepositoryFSErr(t.Context(), slog.New(slog.DiscardHandler), fs, nil))
	})

	t.Run("stable release is pinned with hash and version comment", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		const content = "jobs:\n  build:\n    steps:\n      - uses: actions/checkout@v4.1.0\n"
		setupWorkflow(t, fs, content)
		withMockResolver(t, func(_ context.Context, action, version string) (string, string, error) {
			if action == "actions/checkout" && version == "v4.1.0" {
				return mockHash, "v4.1.0", nil
			}
			return "", "", fmt.Errorf("unexpected: %s@%s", action, version)
		})
		require.NoError(t, patchLocalRepositoryFSErr(t.Context(), slog.New(slog.DiscardHandler), fs, nil))
		got := readWorkflowContent(t, fs)
		require.Contains(t, got, fmt.Sprintf("actions/checkout@%s # v4.1.0", mockHash))
		require.NotContains(t, got, "actions/checkout@v4.1.0")
	})

	t.Run("changed files are reported", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		setupWorkflow(t, fs, "jobs:\n  build:\n    steps:\n      - uses: actions/checkout@v4.1.0\n")
		withMockResolver(t, func(_ context.Context, _, _ string) (string, string, error) {
			return mockHash, "v4.1.0", nil
		})
		changed, err := patchLocalRepositoryFS(t.Context(), slog.New(slog.DiscardHandler), fs, nil)
		require.NoError(t, err)
		require.Equal(t, []string{".github/workflows/ci.yml"}, changed)
	})

	t.Run("no files are reported when everything is pinned", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		setupWorkflow(t, fs, fmt.Sprintf("jobs:\n  build:\n    steps:\n      - uses: actions/checkout@%s\n", mockHash))
		changed, err := patchLocalRepositoryFS(t.Context(), slog.New(slog.DiscardHandler), fs, nil)
		require.NoError(t, err)
		require.Empty(t, changed)
	})

	t.Run("main branch is pinned with release todo comment", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		const content = "jobs:\n  build:\n    steps:\n      - uses: actions/checkout@main\n"
		setupWorkflow(t, fs, content)
		withMockResolver(t, func(_ context.Context, _, _ string) (string, string, error) {
			return mockHash, "main", nil
		})
		require.NoError(t, patchLocalRepositoryFSErr(t.Context(), slog.New(slog.DiscardHandler), fs, nil))
		today := time.Now().Format("2006-01-02")
		require.Contains(t, readWorkflowContent(t, fs), fmt.Sprintf("actions/checkout@%s # main on %s, TODO: use a release instead", mockHash, today))
	})

	t.Run("short version is pinned with consider-release todo comment", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		const content = "jobs:\n  build:\n    steps:\n      - uses: actions/checkout@v4.1\n"
		setupWorkflow(t, fs, content)
		withMockResolver(t, func(_ context.Context, _, _ string) (string, string, error) {
			return mockHash, "v4.1", nil
		})
		require.NoError(t, patchLocalRepositoryFSErr(t.Context(), slog.New(slog.DiscardHandler), fs, nil))
		today := time.Now().Format("2006-01-02")
		require.Contains(t, readWorkflowContent(t, fs), fmt.Sprintf("actions/checkout@%s # v4.1 on %s, TODO: use a release instead", mockHash, today))
	})

	t.Run("unresolved version adds inline todo comment", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		const content = "jobs:\n  build:\n    steps:\n      - uses: actions/checkout@v4\n"
		setupWorkflow(t, fs, content)
		withMockResolver(t, func(_ context.Context, _, _ string) (string, string, error) {
			return "", "", errUnresolvedVersion
		})
		require.NoError(t, patchLocalRepositoryFSErr(t.Context(), slog.New(slog.DiscardHandler), fs, nil))
		require.Contains(t, readWorkflowContent(t, fs), "actions/checkout@v4 # TODO: use a release instead")
	})

	t.Run("resolver error is propagated", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		const content = "jobs:\n  build:\n    steps:\n      - uses: actions/checkout@v4\n"
		setupWorkflow(t, fs, content)
		withMockResolver(t, func(_ context.Context, _, _ string) (string, string, error) {
			return "", "", errors.New("network failure")
		})
		require.Error(t, patchLocalRepositoryFSErr(t.Context(), slog.New(slog.DiscardHandler), fs, nil))
	})
}

func TestPatchLocalRepositoryFSSharedVersionPrefix(t *testing.T) {
	// The result must not depend on which of the two steps is processed first.
	// Steps of a single job are ordered, unlike jobs which are a map.
	for name, content := range map[string]string{
		"short version first": "jobs:\n  a:\n    steps:\n      - uses: actions/checkout@v4\n      - uses: actions/checkout@v4.1.0\n",
		"long version first":  "jobs:\n  a:\n    steps:\n      - uses: actions/checkout@v4.1.0\n      - uses: actions/checkout@v4\n",
	} {
		t.Run(name, func(t *testing.T) {
			fs := afero.NewMemMapFs()
			setupWorkflow(t, fs, content)
			withMockResolver(t, func(_ context.Context, _, version string) (string, string, error) {
				if version == "v4" {
					return "1111111111111111111111111111111111111111", "v4.2.0", nil
				}
				return "2222222222222222222222222222222222222222", version, nil
			})

			require.NoError(t, patchLocalRepositoryFSErr(t.Context(), slog.New(slog.DiscardHandler), fs, nil))

			got := readWorkflowContent(t, fs)
			require.Contains(t, got, "actions/checkout@1111111111111111111111111111111111111111 # v4.2.0\n")
			require.Contains(t, got, "actions/checkout@2222222222222222222222222222222222222222 # v4.1.0\n")
			require.NotContains(t, got, "v4.2.0.1.0")
		})
	}

	t.Run("does not touch an action whose name ends with the same text", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		setupWorkflow(t, fs, "jobs:\n  a:\n    steps:\n      - uses: actions/checkout@v4\n      - uses: other-actions/checkout@v4\n")
		withMockResolver(t, func(_ context.Context, action, _ string) (string, string, error) {
			if action == "actions/checkout" {
				return "1111111111111111111111111111111111111111", "v4.2.0", nil
			}
			return "2222222222222222222222222222222222222222", "v4.3.0", nil
		})

		require.NoError(t, patchLocalRepositoryFSErr(t.Context(), slog.New(slog.DiscardHandler), fs, nil))

		got := readWorkflowContent(t, fs)
		require.Contains(t, got, "- uses: actions/checkout@1111111111111111111111111111111111111111 # v4.2.0\n")
		require.Contains(t, got, "- uses: other-actions/checkout@2222222222222222222222222222222222222222 # v4.3.0\n")
	})
}

func TestReplaceUsesQuoted(t *testing.T) {
	got := replaceUses([]byte("- uses: \"actions/checkout@v4\"\n- uses: 'actions/checkout@v4'\n"), "actions/checkout@v4", "actions/checkout@abc # v4.2.0")
	require.Equal(t, "- uses: \"actions/checkout@abc\" # v4.2.0\n- uses: 'actions/checkout@abc' # v4.2.0\n", string(got))
}

func TestResolveCommitHashWithExecer(t *testing.T) {
	t.Run("fetch tags fails returns error", func(t *testing.T) {
		xr := mock.Execer{
			RunXFn: func(_ context.Context, _ string, _ ...string) (string, error) {
				return "", errors.New("fetch failed")
			},
		}
		_, _, err := resolveCommitHashForVersion(t.Context(), xr, "v4")
		require.Error(t, err)
		require.Contains(t, err.Error(), "fetching tags")
	})

	t.Run("rev-list resolves version directly", func(t *testing.T) {
		xr := mock.Execer{
			RunXFn: func(_ context.Context, _ string, _ ...string) (string, error) {
				return "", nil
			},
			RunFn: func(_ context.Context, _ string, args ...string) (iteratorexec.Result, error) {
				return iteratorexec.Result{Stdout: mockHash + "\n", ExitCode: 0}, nil
			},
		}
		hash, resolvedVersion, err := resolveCommitHashForVersion(t.Context(), xr, "v4.1.0")
		require.NoError(t, err)
		require.Equal(t, mockHash, hash)
		require.Equal(t, "v4.1.0", resolvedVersion)
	})

	t.Run("rev-list run error is wrapped", func(t *testing.T) {
		xr := mock.Execer{
			RunXFn: func(_ context.Context, _ string, _ ...string) (string, error) {
				return "", nil
			},
			RunFn: func(_ context.Context, _ string, _ ...string) (iteratorexec.Result, error) {
				return iteratorexec.Result{}, errors.New("git rev-list failed")
			},
		}
		_, _, err := resolveCommitHashForVersion(t.Context(), xr, "v4.1.0")
		require.Error(t, err)
		require.Contains(t, err.Error(), "getting commit for tag")
	})

	t.Run("rev-list misses full semver returns errUnresolvedVersion", func(t *testing.T) {
		xr := mock.Execer{
			RunXFn: func(_ context.Context, _ string, _ ...string) (string, error) {
				return "", nil
			},
			RunFn: func(_ context.Context, _ string, _ ...string) (iteratorexec.Result, error) {
				return iteratorexec.Result{ExitCode: 1}, nil
			},
		}
		_, _, err := resolveCommitHashForVersion(t.Context(), xr, "v4.1.0")
		require.ErrorIs(t, err, errUnresolvedVersion)
	})

	t.Run("rev-list misses short version resolves via tag list", func(t *testing.T) {
		xr := mock.Execer{
			RunXFn: func(_ context.Context, _ string, _ ...string) (string, error) {
				return "", nil
			},
			RunFn: func(_ context.Context, _ string, args ...string) (iteratorexec.Result, error) {
				switch args[0] {
				case "rev-list":
					if args[len(args)-1] == "refs/tags/v4.1.0" {
						return iteratorexec.Result{Stdout: mockHash + "\n", ExitCode: 0}, nil
					}
					// the floating ref does not exist as a tag nor as a branch
					return iteratorexec.Result{ExitCode: 1}, nil
				case "tag":
					// git sorts by version in descending order
					return iteratorexec.Result{Stdout: "v4.1.0\nv4.0.0\nv4.0.0-beta.1\n", ExitCode: 0}, nil
				default:
					return iteratorexec.Result{}, fmt.Errorf("unexpected git sub-command: %v", args)
				}
			},
		}
		hash, resolvedVersion, err := resolveCommitHashForVersion(t.Context(), xr, "v4")
		require.NoError(t, err)
		require.Equal(t, mockHash, hash)
		require.Equal(t, "v4.1.0", resolvedVersion)
	})

	t.Run("existing floating tag keeps its commit and is named after the release at it", func(t *testing.T) {
		var tagArgs []string
		xr := mock.Execer{
			RunXFn: func(_ context.Context, _ string, _ ...string) (string, error) {
				return "", nil
			},
			RunFn: func(_ context.Context, _ string, args ...string) (iteratorexec.Result, error) {
				switch args[0] {
				case "rev-list":
					require.Equal(t, "refs/tags/v4", args[len(args)-1])
					return iteratorexec.Result{Stdout: mockHash + "\n", ExitCode: 0}, nil
				case "tag":
					tagArgs = args
					return iteratorexec.Result{Stdout: "v4.2.2\n", ExitCode: 0}, nil
				default:
					return iteratorexec.Result{}, fmt.Errorf("unexpected git sub-command: %v", args)
				}
			},
		}
		hash, resolvedVersion, err := resolveCommitHashForVersion(t.Context(), xr, "v4")
		require.NoError(t, err)
		require.Equal(t, mockHash, hash)
		require.Equal(t, "v4.2.2", resolvedVersion)
		require.Equal(t, []string{"tag", "--list", "--sort=-version:refname", "v4.*", "--points-at", mockHash}, tagArgs)
	})

	t.Run("floating tag without a release at its commit keeps the floating ref", func(t *testing.T) {
		xr := mock.Execer{
			RunXFn: func(_ context.Context, _ string, _ ...string) (string, error) {
				return "", nil
			},
			RunFn: func(_ context.Context, _ string, args ...string) (iteratorexec.Result, error) {
				if args[0] == "rev-list" {
					return iteratorexec.Result{Stdout: mockHash + "\n", ExitCode: 0}, nil
				}
				return iteratorexec.Result{}, nil
			},
		}
		_, resolvedVersion, err := resolveCommitHashForVersion(t.Context(), xr, "v4")
		require.NoError(t, err)
		require.Equal(t, "v4", resolvedVersion)
	})

	t.Run("rev-list misses short version tag list empty returns error", func(t *testing.T) {
		xr := mock.Execer{
			RunXFn: func(_ context.Context, _ string, _ ...string) (string, error) {
				return "", nil
			},
			RunFn: func(_ context.Context, _ string, args ...string) (iteratorexec.Result, error) {
				if args[0] == "rev-list" {
					return iteratorexec.Result{ExitCode: 1}, nil
				}
				return iteratorexec.Result{Stdout: "", ExitCode: 0}, nil
			},
		}
		_, _, err := resolveCommitHashForVersion(t.Context(), xr, "v4")
		require.Error(t, err)
		require.ErrorIs(t, err, errUnresolvedVersion)
	})

	t.Run("tag list call fails returns error", func(t *testing.T) {
		xr := mock.Execer{
			RunXFn: func(_ context.Context, _ string, _ ...string) (string, error) {
				return "", nil
			},
			RunFn: func(_ context.Context, _ string, args ...string) (iteratorexec.Result, error) {
				if args[0] == "rev-list" {
					return iteratorexec.Result{ExitCode: 1}, nil
				}
				return iteratorexec.Result{}, errors.New("git tag failed")
			},
		}
		_, _, err := resolveCommitHashForVersion(t.Context(), xr, "v4")
		require.Error(t, err)
		require.Contains(t, err.Error(), "git tag failed")
	})
}

func TestPatchRepository_DryRun(t *testing.T) {
	ctx := context.Background()
	diff := "diff --git a/.github/workflows/ci.yml b/.github/workflows/ci.yml\n-uses: actions/checkout@v4\n+uses: actions/checkout@abc # v4\n"

	cases := []struct {
		name      string
		needsFork bool
	}{
		{"without fork", false},
		{"with fork", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got DryRunDetails
			var gotRepo string

			xr := mock.Execer{
				GenerateFSFn: func() afero.Fs { return workflowMemFS() },
				RunFn: func(ctx context.Context, command string, args ...string) (iteratorexec.Result, error) {
					if command == "git" && args[0] == "status" {
						return iteratorexec.Result{Stdout: "M  .github/workflows/ci.yml\n"}, nil
					}
					return iteratorexec.Result{}, fmt.Errorf("unexpected Run call: %s %v", command, args)
				},
				RunXFn: func(ctx context.Context, command string, args ...string) (string, error) {
					switch {
					case mock.CallIs(t, command, args, "git", "checkout", "-b", defaultTagetBranch):
						return "", nil
					case command == "git" && args[0] == "add":
						return "", nil
					case mock.CallIs(t, command, args, "git", "diff", "--cached", "--name-only"):
						return ".github/workflows/ci.yml\n", nil
					case mock.CallIs(t, command, args, "git", "diff", "--cached"):
						return diff, nil
					default:
						return "", fmt.Errorf("unexpected RunX call (remote write in dry-run?): %s %v", command, args)
					}
				},
			}

			opts := PatchOptions{
				DryRun:    true,
				NeedsFork: tc.needsFork,
				OnPRCreated: func(context.Context, string, PRDetails) {
					require.Fail(t, "PR must not be created in dry-run")
				},
				OnDryRunChanges: func(_ context.Context, repo string, d DryRunDetails) {
					gotRepo, got = repo, d
				},
			}.withDefaults()

			require.NoError(t, patchRepository(ctx, slog.New(slog.DiscardHandler), nonEmptyRepository, xr, opts))
			require.Equal(t, "org/repo", gotRepo)
			require.Equal(t, DryRunDetails{
				Branch: defaultTagetBranch,
				Diff:   diff,
				Files:  []string{".github/workflows/ci.yml"},
			}, got)
		})
	}
}

func TestPatchRepository_DryRun_NoChanges(t *testing.T) {
	xr := mock.Execer{
		GenerateFSFn: func() afero.Fs { return workflowMemFS() },
		RunFn: func(ctx context.Context, command string, args ...string) (iteratorexec.Result, error) {
			return iteratorexec.Result{Stdout: ""}, nil
		},
	}

	var skipped SkippedReason
	opts := PatchOptions{
		DryRun: true,
		OnRepositorySkipped: func(_ context.Context, _ string, r SkippedReason) { skipped = r },
		OnDryRunChanges: func(context.Context, string, DryRunDetails) {
			require.Fail(t, "no dry-run details expected when already pinned")
		},
	}.withDefaults()

	require.NoError(t, patchRepository(context.Background(), slog.New(slog.DiscardHandler), nonEmptyRepository, xr, opts))
	require.Equal(t, SkippedReasonAlreadyPinned, skipped)
}

func TestPatchFSActionDefinitions(t *testing.T) {
	const action = "name: x\nruns:\n  using: composite\n  steps:\n    - uses: actions/checkout@v4.1.0\n"
	setup := func(t *testing.T) afero.Fs {
		fs := afero.NewMemMapFs()
		require.NoError(t, afero.WriteFile(fs, "action.yml", []byte(action), 0644))
		require.NoError(t, afero.WriteFile(fs, ".github/actions/foo/action.yaml", []byte(action), 0644))
		withMockResolver(t, func(_ context.Context, _, _ string) (string, string, error) {
			return mockHash, "v4.1.0", nil
		})
		return fs
	}

	t.Run("ignored by default", func(t *testing.T) {
		fs := setup(t)
		changed, err := patchLocalRepositoryFS(t.Context(), slog.New(slog.DiscardHandler), fs, nil)
		require.NoError(t, err)
		require.Empty(t, changed)
		b, _ := afero.ReadFile(fs, "action.yml")
		require.Equal(t, action, string(b))
	})

	t.Run("pinned when enabled", func(t *testing.T) {
		fs := setup(t)
		changed, err := patchFS(t.Context(), slog.New(slog.DiscardHandler), fs, nil, true)
		require.NoError(t, err)
		require.Equal(t, []string{"action.yml", ".github/actions/foo/action.yaml"}, changed)
		b, _ := afero.ReadFile(fs, "action.yml")
		require.Contains(t, string(b), "actions/checkout@"+mockHash+" # v4.1.0")
	})
}

func TestPatchLocalRepositoryFSReusableWorkflows(t *testing.T) {
	const (
		calledHash = "1111111111111111111111111111111111111111"
		pinnedHash = "2222222222222222222222222222222222222222"
	)

	tests := []struct {
		name        string
		uses        string
		trustedOrgs []string
		want        string
	}{
		{
			name: "third party reusable workflow is pinned",
			uses: "other-org/shared/.github/workflows/build.yml@v1",
			want: "other-org/shared/.github/workflows/build.yml@" + calledHash + " # v1.4.0",
		},
		{
			name:        "trusted org reusable workflow is skipped",
			uses:        "jcchavezs/shared/.github/workflows/build.yml@v1",
			trustedOrgs: []string{"jcchavezs"},
			want:        "jcchavezs/shared/.github/workflows/build.yml@v1",
		},
		{
			name: "local reusable workflow is skipped",
			uses: "./.github/workflows/build.yml",
			want: "./.github/workflows/build.yml",
		},
		{
			name: "already pinned reusable workflow is skipped",
			uses: "other-org/shared/.github/workflows/build.yml@" + pinnedHash,
			want: "other-org/shared/.github/workflows/build.yml@" + pinnedHash,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := afero.NewMemMapFs()
			content := "jobs:\n  build:\n    uses: %s\n    secrets: inherit\n"
			setupWorkflow(t, fs, fmt.Sprintf(content, tt.uses))
			withMockResolver(t, func(_ context.Context, action, version string) (string, string, error) {
				if action == "other-org/shared/.github/workflows/build.yml" && version == "v1" {
					return calledHash, "v1.4.0", nil
				}
				return "", "", fmt.Errorf("unexpected: %s@%s", action, version)
			})

			changed, err := patchLocalRepositoryFS(t.Context(), slog.New(slog.DiscardHandler), fs, tt.trustedOrgs)
			require.NoError(t, err)
			require.Equal(t, fmt.Sprintf(content, tt.want), readWorkflowContent(t, fs))
			require.Equal(t, tt.want != tt.uses, len(changed) == 1)
		})
	}

	t.Run("reusable workflow and step actions are pinned together", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		setupWorkflow(t, fs, "jobs:\n  call:\n    uses: other-org/shared/.github/workflows/build.yml@v1\n  test:\n    steps:\n      - uses: actions/checkout@v4\n")
		withMockResolver(t, func(_ context.Context, action, _ string) (string, string, error) {
			if action == "actions/checkout" {
				return pinnedHash, "v4.2.0", nil
			}
			return calledHash, "v1.4.0", nil
		})

		changed, err := patchLocalRepositoryFS(t.Context(), slog.New(slog.DiscardHandler), fs, nil)
		require.NoError(t, err)
		require.Equal(t, []string{".github/workflows/ci.yml"}, changed)

		got := readWorkflowContent(t, fs)
		require.Contains(t, got, "uses: other-org/shared/.github/workflows/build.yml@"+calledHash+" # v1.4.0\n")
		require.Contains(t, got, "- uses: actions/checkout@"+pinnedHash+" # v4.2.0\n")
	})

	t.Run("reusable workflow without version returns error", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		setupWorkflow(t, fs, "jobs:\n  call:\n    uses: other-org/shared/.github/workflows/build.yml\n")
		require.Error(t, patchLocalRepositoryFSErr(t.Context(), slog.New(slog.DiscardHandler), fs, nil))
	})
}

// TestResolveCommitHashForVersionRealGit runs the resolution against a real repository, as the
// ordering and matching of tags is git behaviour that mocks cannot prove.
func TestResolveCommitHashForVersionRealGit(t *testing.T) {
	git := func(t *testing.T, dir string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{
			"-c", "user.name=test", "-c", "user.email=test@example.com",
			"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false",
		}, args...)...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		return strings.TrimSpace(string(out))
	}

	// release creates a commit and tags it with the given tags, returning its hash.
	release := func(t *testing.T, dir string, tags ...string) string {
		t.Helper()
		git(t, dir, "commit", "--allow-empty", "-m", strings.Join(tags, ","))
		for _, tag := range tags {
			git(t, dir, "tag", "-f", tag)
		}
		return git(t, dir, "rev-parse", "HEAD")
	}

	// A history in which alphabetical order disagrees with version order.
	origin := t.TempDir()
	git(t, origin, "init", "-q", "-b", "main")
	v12 := release(t, origin, "v1.2.0", "v1.2")
	release(t, origin, "v1.42.0")
	v1100 := release(t, origin, "v1.100.0", "v1")
	v10 := release(t, origin, "v10.0.0")
	release(t, origin, "v1-beta")
	// there is no floating tag for v4, so it has to be searched
	release(t, origin, "v4.2.2")
	v410 := release(t, origin, "v4.10.0")

	clone := t.TempDir()
	git(t, clone, "clone", "-q", origin, ".")

	tests := []struct {
		name        string
		version     string
		wantHash    string
		wantVersion string
	}{
		{"floating tag keeps its commit and is named after its release", "v1", v1100, "v1.100.0"},
		{"floating minor tag", "v1.2", v12, "v1.2.0"},
		{"searched floating ref picks the highest version, not the last alphabetically", "v4", v410, "v4.10.0"},
		{"searched floating ref does not match other majors", "v10", v10, "v10.0.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hash, resolvedVersion, err := resolveCommitHashForVersion(t.Context(), iteratorexec.NewExecer(clone), tt.version)
			require.NoError(t, err)
			require.Equal(t, tt.wantHash, hash)
			require.Equal(t, tt.wantVersion, resolvedVersion)
		})
	}

	t.Run("a floating ref with no release tags is unresolved", func(t *testing.T) {
		_, _, err := resolveCommitHashForVersion(t.Context(), iteratorexec.NewExecer(clone), "v2")
		require.ErrorIs(t, err, errUnresolvedVersion)
	})
}
