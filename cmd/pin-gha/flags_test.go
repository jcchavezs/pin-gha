package main

import (
	"io"
	"testing"

	"github.com/jcchavezs/pin-gha/patch"
	"github.com/stretchr/testify/require"
)

func TestVersionAlignmentFlagsAreMutuallyExclusive(t *testing.T) {
	for _, command := range []string{"repository", "organization", "local-repository"} {
		t.Run(command, func(t *testing.T) {
			rootCmd.SetOut(io.Discard)
			rootCmd.SetErr(io.Discard)
			rootCmd.SetArgs([]string{command, "target", "--major-version-alignment", "--all-version-alignment"})
			t.Cleanup(func() { rootCmd.SetArgs(nil) })
			t.Cleanup(resetVersionAlignmentFlags)

			require.ErrorContains(t, rootCmd.Execute(), "none of the others can be")
		})
	}
}

func TestVersionAlignment(t *testing.T) {
	resetVersionAlignmentFlags()
	t.Cleanup(resetVersionAlignmentFlags)

	require.Equal(t, patch.VersionAlignmentNone, versionAlignment())

	patchFlags.majorVersionAlignment = true
	require.Equal(t, patch.VersionAlignmentMajor, versionAlignment())

	patchFlags.majorVersionAlignment, patchFlags.allVersionAlignment = false, true
	require.Equal(t, patch.VersionAlignmentAll, versionAlignment())
}

// resetVersionAlignmentFlags undoes what parsing flags leaves in the shared state.
func resetVersionAlignmentFlags() {
	patchFlags.majorVersionAlignment, patchFlags.allVersionAlignment = false, false
}
