package patch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	iteratorexec "github.com/jcchavezs/gh-iterator/exec"
	"github.com/jcchavezs/gh-iterator/exec/mock"
	"github.com/spf13/afero"
	"github.com/stretchr/testify/require"
)

func TestCheckCommandExist(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "test")
	require.NoError(t, err, "Failed to create temp dir")

	defer os.RemoveAll(tempDir) //nolint

	originPath := os.Getenv("PATH")
	defer os.Setenv("PATH", originPath) //nolint

	// Setup Test PATH
	os.Setenv("PATH", tempDir) //nolint

	t.Run("Existing command", func(t *testing.T) {
		fakeCmdPath := filepath.Join(tempDir, "thisIsFakeCmd")
		err = os.WriteFile(fakeCmdPath, []byte("#!/bin/sh\necho 'fake command'"), 0755)
		require.NoError(t, err, "Failed to create fake command")

		defer func() {
			if r := recover(); r != nil {
				t.Errorf("Existing command should not panic: %v", r)
			}
		}()

		checkCommandExist("thisIsFakeCmd")
	})

	t.Run("Non-existing command", func(t *testing.T) {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("Non-Existing command should panic: %v", r)
			}
		}()
		checkCommandExist("thisIsFakeCmd")
	})
}

// emptyMemFS returns an afero.MemMapFs with no .github/workflows directory,
// so patchLocalRepositoryFS returns immediately with no changes.
func emptyMemFS() afero.Fs {
	return afero.NewMemMapFs()
}

// TestPatchRepository_NoChanges verifies that when HasChanges returns false
// (git status -s produces empty output), patchRepository returns nil without
// attempting to create a branch or PR.
func TestPatchRepository_NoChanges(t *testing.T) {
	ctx := context.Background()

	xr := mock.Execer{
		// GenerateFS returns an empty FS — no .github/workflows, so no patches are applied.
		GenerateFSFn: func() afero.Fs {
			return emptyMemFS()
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

	require.NoError(t, patchRepository(ctx, "org/repo", false, xr, PatchOptions{}))
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
			return emptyMemFS()
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

	require.NoError(t, patchRepository(ctx, "org/repo", false, xr, PatchOptions{}))
	require.Equal(t, defaultTagetBranch, checkedOutBranch)
	require.Equal(t, ".github/workflows", addedPath)
	require.Equal(t, "chore(security): uses pinned versions of actions", commitMessage)
	require.Equal(t, defaultTagetBranch, pushedBranch)
}

// TestPatchRepository_HasChanges_PRAlreadyExists verifies that when an open PR
// already exists, patchRepository succeeds (updates the PR) without creating a new one.
func TestPatchRepository_HasChanges_PRAlreadyExists(t *testing.T) {
	ctx := context.Background()

	xr := mock.Execer{
		GenerateFSFn: func() afero.Fs {
			return emptyMemFS()
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

	require.NoError(t, patchRepository(ctx, "org/repo", false, xr, PatchOptions{}))
}

// TestPatchRepository_HasChangesError verifies that an error from HasChanges
// is propagated back to the caller.
func TestPatchRepository_HasChangesError(t *testing.T) {
	ctx := context.Background()

	xr := mock.Execer{
		GenerateFSFn: func() afero.Fs {
			return emptyMemFS()
		},
		RunFn: func(ctx context.Context, command string, args ...string) (iteratorexec.Result, error) {
			return iteratorexec.Result{}, errors.New("git status failed")
		},
	}

	require.Error(t, patchRepository(ctx, "org/repo", false, xr, PatchOptions{}))
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

func TestPatchLocalRepositoryFS(t *testing.T) {
	t.Run("no workflows directory returns nil", func(t *testing.T) {
		require.NoError(t, patchLocalRepositoryFS(t.Context(), afero.NewMemMapFs(), PatchOptions{}))
	})

	t.Run("non yaml files are ignored", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		require.NoError(t, fs.MkdirAll(".github/workflows", 0755))
		require.NoError(t, afero.WriteFile(fs, ".github/workflows/notes.txt", []byte("not yaml"), 0644))
		require.NoError(t, patchLocalRepositoryFS(t.Context(), fs, PatchOptions{}))
		// Verify the non-yaml file is unchanged.
		b, err := afero.ReadFile(fs, ".github/workflows/notes.txt")
		require.NoError(t, err)
		require.Equal(t, "not yaml", string(b))
	})

	t.Run("step with empty uses is skipped", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		const content = "jobs:\n  build:\n    steps:\n      - name: Run something\n"
		setupWorkflow(t, fs, content)
		require.NoError(t, patchLocalRepositoryFS(t.Context(), fs, PatchOptions{}))
		require.Equal(t, content, readWorkflowContent(t, fs))
	})

	t.Run("trusted org action is skipped", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		const content = "jobs:\n  build:\n    steps:\n      - uses: jcchavezs/my-action@v1.2.3\n"
		setupWorkflow(t, fs, content)
		require.NoError(t, patchLocalRepositoryFS(t.Context(), fs, PatchOptions{
			TrustedOrgs: []string{"jcchavezs"},
		}))
		require.Equal(t, content, readWorkflowContent(t, fs))
	})

	t.Run("local action with existing path is skipped", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		const content = "jobs:\n  build:\n    steps:\n      - uses: ./local-action\n"
		setupWorkflow(t, fs, content)
		// afero.Exists normalises ./local-action → local-action; create the dir.
		require.NoError(t, fs.MkdirAll("local-action", 0755))
		require.NoError(t, patchLocalRepositoryFS(t.Context(), fs, PatchOptions{}))
		require.Equal(t, content, readWorkflowContent(t, fs))
	})

	t.Run("already pinned 40-char hash is skipped", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		content := fmt.Sprintf("jobs:\n  build:\n    steps:\n      - uses: actions/checkout@%s\n", mockHash)
		setupWorkflow(t, fs, content)
		require.NoError(t, patchLocalRepositoryFS(t.Context(), fs, PatchOptions{}))
		require.Equal(t, content, readWorkflowContent(t, fs))
	})

	t.Run("missing version returns error", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		const content = "jobs:\n  build:\n    steps:\n      - uses: actions/checkout\n"
		setupWorkflow(t, fs, content)
		require.Error(t, patchLocalRepositoryFS(t.Context(), fs, PatchOptions{}))
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
		require.NoError(t, patchLocalRepositoryFS(t.Context(), fs, PatchOptions{}))
		got := readWorkflowContent(t, fs)
		require.Contains(t, got, fmt.Sprintf("actions/checkout@%s # v4.1.0", mockHash))
		require.NotContains(t, got, "actions/checkout@v4.1.0")
	})

	t.Run("main branch is pinned with release todo comment", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		const content = "jobs:\n  build:\n    steps:\n      - uses: actions/checkout@main\n"
		setupWorkflow(t, fs, content)
		withMockResolver(t, func(_ context.Context, _, _ string) (string, string, error) {
			return mockHash, "main", nil
		})
		require.NoError(t, patchLocalRepositoryFS(t.Context(), fs, PatchOptions{}))
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
		require.NoError(t, patchLocalRepositoryFS(t.Context(), fs, PatchOptions{}))
		today := time.Now().Format("2006-01-02")
		require.Contains(t, readWorkflowContent(t, fs), fmt.Sprintf("actions/checkout@%s # v4.1 on %s, TODO: consider using a release", mockHash, today))
	})

	t.Run("unresolved version adds inline todo comment", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		const content = "jobs:\n  build:\n    steps:\n      - uses: actions/checkout@v4\n"
		setupWorkflow(t, fs, content)
		withMockResolver(t, func(_ context.Context, _, _ string) (string, string, error) {
			return "", "", errUnresolvedVersion
		})
		require.NoError(t, patchLocalRepositoryFS(t.Context(), fs, PatchOptions{}))
		require.Contains(t, readWorkflowContent(t, fs), "actions/checkout@v4 # TODO: use a release instead")
	})

	t.Run("resolver error is propagated", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		const content = "jobs:\n  build:\n    steps:\n      - uses: actions/checkout@v4\n"
		setupWorkflow(t, fs, content)
		withMockResolver(t, func(_ context.Context, _, _ string) (string, string, error) {
			return "", "", errors.New("network failure")
		})
		require.Error(t, patchLocalRepositoryFS(t.Context(), fs, PatchOptions{}))
	})
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
					if args[len(args)-1] == "v4" {
						// first attempt: version not found as a direct tag
						return iteratorexec.Result{ExitCode: 1}, nil
					}
					// second attempt after tag expansion: v4.1.0 found
					return iteratorexec.Result{Stdout: mockHash + "\n", ExitCode: 0}, nil
				case "tag":
					return iteratorexec.Result{Stdout: "v4.0.0\nv4.1.0", ExitCode: 0}, nil
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
