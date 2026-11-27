package patch

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	iterator "github.com/jcchavezs/gh-iterator"
	iteratorexec "github.com/jcchavezs/gh-iterator/exec"
	"github.com/jcchavezs/gh-iterator/exec/mock"
	"github.com/spf13/afero"
	"github.com/stretchr/testify/require"
)

const (
	hashV420 = "2222222222222222222222222222222222222222"
	hashV430 = "3333333333333333333333333333333333333333"
	hashRC   = "4444444444444444444444444444444444444444"
)

// writeWorkflows writes one workflow per entry of uses, each with a single step.
func writeWorkflows(t *testing.T, fs afero.Fs, uses map[string]string) {
	t.Helper()
	require.NoError(t, fs.MkdirAll(".github/workflows", 0755))
	for name, use := range uses {
		content := fmt.Sprintf("jobs:\n  build:\n    steps:\n      - uses: %s\n", use)
		require.NoError(t, afero.WriteFile(fs, ".github/workflows/"+name, []byte(content), 0644))
	}
}

func readFile(t *testing.T, fs afero.Fs, name string) string {
	t.Helper()
	b, err := afero.ReadFile(fs, name)
	require.NoError(t, err)
	return string(b)
}

// withReleases mocks the resolver with the given declared version to resolved version/hash.
func withReleases(t *testing.T, releases map[string][2]string) {
	t.Helper()
	withMockResolver(t, func(_ context.Context, _, version string) (string, string, error) {
		r, ok := releases[version]
		require.True(t, ok, "unexpected version: %s", version)
		return r[0], r[1], nil
	})
}

func TestActionRepository(t *testing.T) {
	for action, want := range map[string]string{
		"actions/checkout":                 "actions/checkout",
		"github/codeql-action/init":        "github/codeql-action",
		"org/repo/.github/workflows/a.yml": "org/repo",
		"checkout":                         "checkout",
	} {
		require.Equal(t, want, actionRepository(action), action)
	}
}

func TestPatchFSAlignmentIgnoresPreReleases(t *testing.T) {
	withReleases(t, map[string][2]string{
		"v4.2.0":      {hashV420, "v4.2.0"},
		"v4.3.0-rc.1": {hashRC, "v4.3.0-rc.1"},
	})

	for _, alignment := range []VersionAlignment{VersionAlignmentMajor, VersionAlignmentAll} {
		t.Run(fmt.Sprint(alignment), func(t *testing.T) {
			fs := afero.NewMemMapFs()
			writeWorkflows(t, fs, map[string]string{"a.yml": "actions/checkout@v4.2.0", "b.yml": "actions/checkout@v4.3.0-rc.1"})

			_, err := patchLocalRepositoryFS(t.Context(), slog.New(slog.DiscardHandler), fs, nil, alignment)
			require.NoError(t, err)

			require.Contains(t, readFile(t, fs, ".github/workflows/a.yml"), "actions/checkout@"+hashV420+" # v4.2.0")
			require.Contains(t, readFile(t, fs, ".github/workflows/b.yml"), "actions/checkout@"+hashRC+" # v4.3.0-rc.1")
		})
	}
}

func TestPatchFSAlignmentAlignsActionsOfTheSameRepository(t *testing.T) {
	withMockResolver(t, func(_ context.Context, action, version string) (string, string, error) {
		if version == "v3.2.0" {
			return hashV420, "v3.2.0", nil
		}
		return hashV430, "v3.1.0", nil
	})

	fs := afero.NewMemMapFs()
	writeWorkflows(t, fs, map[string]string{
		"a.yml": "github/codeql-action/init@v3.2.0",
		"b.yml": "github/codeql-action/analyze@v3.1.0",
		"c.yml": "other/codeql-action/analyze@v3.1.0",
	})

	_, err := patchLocalRepositoryFS(t.Context(), slog.New(slog.DiscardHandler), fs, nil, VersionAlignmentMajor)
	require.NoError(t, err)

	require.Contains(t, readFile(t, fs, ".github/workflows/a.yml"), "github/codeql-action/init@"+hashV420+" # v3.2.0")
	require.Contains(t, readFile(t, fs, ".github/workflows/b.yml"), "github/codeql-action/analyze@"+hashV420+" # v3.2.0")
	require.Contains(t, readFile(t, fs, ".github/workflows/c.yml"), "other/codeql-action/analyze@"+hashV430+" # v3.1.0", "a different repository is not aligned")
}

func TestPatchFSAlignmentSkipsUsesWithoutVersion(t *testing.T) {
	today := time.Now().Format("2006-01-02")
	withMockResolver(t, func(_ context.Context, _, version string) (string, string, error) {
		if version == "" {
			return mockHash, "main", nil
		}
		return hashV420, "v4.2.0", nil
	})

	fs := afero.NewMemMapFs()
	writeWorkflows(t, fs, map[string]string{"a.yml": "actions/checkout", "b.yml": "actions/checkout@v4.2.0"})

	_, err := patchLocalRepositoryFS(t.Context(), slog.New(slog.DiscardHandler), fs, nil, VersionAlignmentAll)
	require.NoError(t, err)

	require.Contains(t, readFile(t, fs, ".github/workflows/a.yml"), "actions/checkout@"+mockHash+" # main on "+today+", TODO: use a release instead")
	require.Contains(t, readFile(t, fs, ".github/workflows/b.yml"), "actions/checkout@"+hashV420+" # v4.2.0")
}

func TestPatchFSAlignmentAcrossWorkflowsAndActionDefinitions(t *testing.T) {
	withReleases(t, map[string][2]string{
		"v4.1.0": {mockHash, "v4.1.0"},
		"v4.2.0": {hashV420, "v4.2.0"},
	})

	fs := afero.NewMemMapFs()
	writeWorkflows(t, fs, map[string]string{"ci.yml": "actions/checkout@v4.1.0"})
	require.NoError(t, afero.WriteFile(fs, "action.yml", []byte("runs:\n  using: composite\n  steps:\n    - uses: actions/checkout@v4.2.0\n"), 0644))

	changed, err := patchFS(t.Context(), slog.New(slog.DiscardHandler), fs, nil, true, VersionAlignmentMajor)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{".github/workflows/ci.yml", "action.yml"}, changed)
	require.Contains(t, readFile(t, fs, ".github/workflows/ci.yml"), "actions/checkout@"+hashV420+" # v4.2.0")

	t.Run("not aligned when action definitions are excluded", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		writeWorkflows(t, fs, map[string]string{"ci.yml": "actions/checkout@v4.1.0"})
		require.NoError(t, afero.WriteFile(fs, "action.yml", []byte("runs:\n  using: composite\n  steps:\n    - uses: actions/checkout@v4.2.0\n"), 0644))

		_, err := patchFS(t.Context(), slog.New(slog.DiscardHandler), fs, nil, false, VersionAlignmentMajor)
		require.NoError(t, err)
		require.Contains(t, readFile(t, fs, ".github/workflows/ci.yml"), "actions/checkout@"+mockHash+" # v4.1.0")
	})
}

func TestLocalRepositoryVersionAlignment(t *testing.T) {
	withReleases(t, map[string][2]string{
		"v4.1.0": {mockHash, "v4.1.0"},
		"v4.2.0": {hashV420, "v4.2.0"},
	})

	dir := t.TempDir()
	fs := afero.NewBasePathFs(afero.NewOsFs(), dir)
	writeWorkflows(t, fs, map[string]string{"a.yml": "actions/checkout@v4.1.0", "b.yml": "actions/checkout@v4.2.0"})

	var changed []string
	require.NoError(t, LocalRepository(t.Context(), dir, LocalPatchOptions{
		VersionAlignment: VersionAlignmentMajor,
		OnFileChanged:    func(_ context.Context, file string) { changed = append(changed, file) },
	}))

	require.ElementsMatch(t, []string{".github/workflows/a.yml", ".github/workflows/b.yml"}, changed)
	require.Contains(t, readFile(t, fs, ".github/workflows/a.yml"), "actions/checkout@"+hashV420+" # v4.2.0")
	require.Contains(t, readFile(t, fs, ".github/workflows/b.yml"), "actions/checkout@"+hashV420+" # v4.2.0")
}

func TestPatchRepositoryVersionAlignment(t *testing.T) {
	withReleases(t, map[string][2]string{
		"v4.1.0": {mockHash, "v4.1.0"},
		"v4.2.0": {hashV420, "v4.2.0"},
	})

	fs := afero.NewMemMapFs()
	writeWorkflows(t, fs, map[string]string{"a.yml": "actions/checkout@v4.1.0", "b.yml": "actions/checkout@v4.2.0"})

	xr := mock.Execer{
		GenerateFSFn: func() afero.Fs { return fs },
		RunFn: func(_ context.Context, command string, args ...string) (iteratorexec.Result, error) {
			if command == "git" && args[0] == "status" {
				return iteratorexec.Result{Stdout: "M  .github/workflows/a.yml\n"}, nil
			}
			return iteratorexec.Result{ExitCode: 1}, nil
		},
		RunXFn: func(context.Context, string, ...string) (string, error) { return "", nil },
	}

	opts := PatchOptions{VersionAlignment: VersionAlignmentMajor}.withDefaults()
	require.NoError(t, patchRepository(t.Context(), slog.New(slog.DiscardHandler), iterator.Repository{Name: "org/repo", Size: 1}, xr, opts))

	require.Contains(t, readFile(t, fs, ".github/workflows/a.yml"), "actions/checkout@"+hashV420+" # v4.2.0")
}
