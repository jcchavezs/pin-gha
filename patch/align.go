package patch

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"
)

// VersionAlignment selects how the versions of the same action are aligned
// across the workflows of a repository. The highest resolved release version
// of the group wins; references that are not release versions, such as
// branches, are never aligned.
type VersionAlignment int

const (
	// VersionAlignmentNone pins every use to the version it already declares.
	VersionAlignmentNone VersionAlignment = iota
	// VersionAlignmentMajor aligns the uses of an action within each major version.
	VersionAlignmentMajor
	// VersionAlignmentAll aligns all the uses of an action, whatever their major version.
	VersionAlignmentAll
)

// actionVersion is an action as declared in a workflow, before resolving it.
type actionVersion struct {
	action  string
	version string
}

type resolvedAction struct {
	hash    string
	version string
	err     error
}

// alignmentKey identifies a group of resolved versions that are aligned together.
type alignmentKey struct {
	action string
	// major is only set when aligning per major version.
	major int
}

// alignmentGroup returns the group a resolved version belongs to. ok is false
// when it must be left as resolved: no alignment is requested or the version
// is not a release (e.g. a branch).
func alignmentGroup(action, version string, alignment VersionAlignment) (key alignmentKey, ok bool) {
	if alignment == VersionAlignmentNone {
		return alignmentKey{}, false
	}
	numbers, ok := versionNumbers(version)
	if !ok {
		return alignmentKey{}, false
	}
	key = alignmentKey{action: action}
	if alignment == VersionAlignmentMajor {
		key.major = numbers[0]
	}
	return key, true
}

// highestVersions returns the highest resolved version of every alignment group.
func highestVersions(refs []actionVersion, resolutions map[actionVersion]resolvedAction, alignment VersionAlignment) map[alignmentKey]resolvedAction {
	highest := make(map[alignmentKey]resolvedAction)
	for _, ref := range refs {
		resolved := resolutions[ref]
		if resolved.err != nil {
			continue
		}
		group, ok := alignmentGroup(ref.action, resolved.version, alignment)
		if !ok {
			continue
		}
		if current, exists := highest[group]; !exists || isNewerVersion(resolved, current) {
			highest[group] = resolved
		}
	}
	return highest
}

// alignedResolution returns the resolution a use must be pinned to.
func alignedResolution(logger *slog.Logger, action string, resolved resolvedAction, highest map[alignmentKey]resolvedAction, alignment VersionAlignment) resolvedAction {
	group, ok := alignmentGroup(action, resolved.version, alignment)
	if !ok {
		if alignment != VersionAlignmentNone {
			logger.Debug("Not aligning a reference that is not a release", "action", action, "version", resolved.version)
		}
		return resolved
	}
	if aligned := highest[group]; aligned.version != resolved.version {
		logger.Debug("Aligning action version", "action", action, "from", resolved.version, "to", aligned.version)
		return aligned
	}
	return resolved
}

// versionNumbers parses the numbers of a release version such as "v4.2.0".
// Pre-release and build suffixes are ignored.
func versionNumbers(version string) ([3]int, bool) {
	var numbers [3]int
	version = strings.TrimPrefix(version, "v")
	version, _, _ = strings.Cut(version, "-")
	version, _, _ = strings.Cut(version, "+")
	parts := strings.Split(version, ".")
	if len(parts) > len(numbers) {
		return numbers, false
	}
	for i, part := range parts {
		number, err := strconv.Atoi(part)
		if err != nil {
			return numbers, false
		}
		numbers[i] = number
	}
	return numbers, true
}

// isNewerVersion reports whether candidate is a higher release than current.
// Both must be release versions. Equal numbers are ordered by the version
// string, so "v1.0.0" wins over "v1.0.0-rc.1", and by hash, which makes the
// result independent of the order the workflows are read in.
func isNewerVersion(candidate, current resolvedAction) bool {
	candidateNumbers, _ := versionNumbers(candidate.version)
	currentNumbers, _ := versionNumbers(current.version)
	for i := range candidateNumbers {
		if candidateNumbers[i] != currentNumbers[i] {
			return candidateNumbers[i] > currentNumbers[i]
		}
	}
	if candidate.version != current.version {
		return candidate.version < current.version
	}
	return candidate.hash < current.hash
}

// pinnedUses returns the value a use is replaced with.
func pinnedUses(action string, resolved resolvedAction, today string) string {
	if resolved.version == "master" || resolved.version == "main" || strings.Count(resolved.version, ".") < 2 {
		return fmt.Sprintf("%s@%s # %s on %s, TODO: use a release instead", action, resolved.hash, resolved.version, today)
	}
	return fmt.Sprintf("%s@%s # %s", action, resolved.hash, resolved.version)
}
