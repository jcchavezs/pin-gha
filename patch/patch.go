package patch

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"time"

	iterator "github.com/jcchavezs/gh-iterator"
	iteratorexec "github.com/jcchavezs/gh-iterator/exec"
	"github.com/jcchavezs/gh-iterator/github"
	"github.com/spf13/afero"
	"gopkg.in/yaml.v3"
)

type workflow struct {
	Name string `yaml:"name"`
	Jobs map[string]struct {
		Name  string `yaml:"name"`
		Steps []struct {
			Name string `yaml:"name"`
			Uses string `yaml:"uses"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

func init() {
	checkCommandExist("git")
	checkCommandExist("gh")
}

func checkCommandExist(path string) {
	_, err := exec.LookPath(path)
	if err != nil {
		panic(fmt.Sprintf("Failed to find '%s' command. Please install it and try again.\n", path))
	}
}

// Organization processes all repositories in the given organization.
func Organization(ctx context.Context, orgName string, opts PatchOptions) error {
	if err := validateVersionAlignment(opts.MajorVersionAlignment, opts.AllVersionAlignment); err != nil {
		return err
	}
	opts = opts.withDefaults()

	_, err := iterator.RunForOrganization(ctx, orgName, iterator.SearchOptions{
		ArchiveCondition: iterator.OmitArchived,
		SizeCondition:    iterator.NotEmpty,
		Source:           iterator.OnlyNonForks,
		Page:             iterator.AllPages,
	}, func(ctx context.Context, repo string, isEmpty bool, xr iteratorexec.Execer) error {
		if err := patchRepository(ctx, slog.New(opts.LogHandler), repo, isEmpty, xr, opts); err != nil {
			return opts.OnRepositoryErr(ctx, repo, err)
		}

		return nil
	}, iterator.Options{
		UseHTTPS:      true,
		LogHandler:    opts.LogHandler,
		CloningSubset: []string{".github/workflows"},
	})

	return err
}

// Repository processes a single repository.
func Repository(ctx context.Context, repo string, opts PatchOptions) error {
	if err := validateVersionAlignment(opts.MajorVersionAlignment, opts.AllVersionAlignment); err != nil {
		return err
	}
	opts = opts.withDefaults()

	return iterator.RunForRepository(ctx, repo, func(ctx context.Context, repo string, isEmpty bool, xr iteratorexec.Execer) error {
		if err := patchRepository(ctx, slog.New(opts.LogHandler), repo, isEmpty, xr, opts); err != nil {
			return opts.OnRepositoryErr(ctx, repo, err)
		}

		return nil
	}, iterator.Options{
		UseHTTPS:      true,
		LogHandler:    opts.LogHandler,
		CloningSubset: []string{".github/workflows"},
	})
}

// LocalRepository processes a local repository at the given path. This is used for testing and can also be used by users who want to apply the patch to a local repository without creating a PR.
func LocalRepository(ctx context.Context, repoPath string, opts LocalPatchOptions) error {
	opts = opts.withDefaults()
	return patchLocalRepositoryFSWithAlignment(ctx, slog.New(opts.LogHandler), afero.NewBasePathFs(afero.NewOsFs(), repoPath), opts.TrustedOrgs, opts.MajorVersionAlignment, opts.AllVersionAlignment)
}

// patchLocalRepositoryFS applies the patch to the given FS. This is used for testing.
func patchLocalRepositoryFS(ctx context.Context, logger *slog.Logger, fs afero.Fs, trustedOrgs []string) error {
	return patchLocalRepositoryFSWithAlignment(ctx, logger, fs, trustedOrgs, false, false)
}

type actionVersion struct {
	action  string
	version string
}

type resolvedAction struct {
	hash    string
	version string
	err     error
}

type workflowFile struct {
	path    string
	content []byte
	workflow
}

func validateVersionAlignment(majorVersionAlignment, allVersionAlignment bool) error {
	if majorVersionAlignment && allVersionAlignment {
		return errors.New("--major-version-alignment and --all-version-alignment are mutually exclusive")
	}
	return nil
}

func patchLocalRepositoryFSWithAlignment(ctx context.Context, logger *slog.Logger, fs afero.Fs, trustedOrgs []string, majorVersionAlignment, allVersionAlignment bool) error {
	if err := validateVersionAlignment(majorVersionAlignment, allVersionAlignment); err != nil {
		return err
	}

	today := time.Now().Format("2006-01-02")
	files, err := afero.ReadDir(fs, path.Join(".github", "workflows"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	if len(files) == 0 {
		logger.Debug("No files found")
	}

	var workflows []workflowFile
	resolutions := make(map[actionVersion]resolvedAction)
	var actionVersions []actionVersion
	for _, file := range files {
		if file.IsDir() {
			continue
		}

		if !strings.HasSuffix(file.Name(), ".yml") && !strings.HasSuffix(file.Name(), ".yaml") {
			continue
		}

		workflowPath := path.Join(".github", "workflows", file.Name())
		wfb, err := afero.ReadFile(fs, workflowPath)
		if err != nil {
			return err
		}

		wf := workflow{}
		if err := yaml.Unmarshal(wfb, &wf); err != nil {
			return fmt.Errorf("unmarshalling workflow %q: %v", file.Name(), err)
		}

		workflows = append(workflows, workflowFile{path: workflowPath, content: wfb, workflow: wf})
		for _, job := range wf.Jobs {
			for _, step := range job.Steps {
				uses := strings.TrimSpace(step.Uses)
				if uses == "" {
					continue
				}

				if isTrustedAction(uses, trustedOrgs) || strings.HasPrefix(uses, "./") {
					continue
				}

				pkg, version, _ := strings.Cut(uses, "@")

				if version == "" {
					return fmt.Errorf("no version specified for action %s in %q", pkg, file.Name())
				}

				if len(version) == 40 { // commit hash length
					continue
				}

				ref := actionVersion{action: pkg, version: version}
				if _, exists := resolutions[ref]; exists {
					continue
				}

				hash, resolvedVersion, resolveErr := resolveCommitHash(ctx, pkg, version)
				if resolveErr != nil && resolveErr != errUnresolvedVersion {
					return fmt.Errorf("getting commit hash: %v", resolveErr)
				}
				resolutions[ref] = resolvedAction{hash: hash, version: resolvedVersion, err: resolveErr}
				actionVersions = append(actionVersions, ref)
			}
		}
	}

	alignedResolutions := make(map[alignmentKey]resolvedAction)
	if majorVersionAlignment || allVersionAlignment {
		for _, ref := range actionVersions {
			resolved := resolutions[ref]
			if resolved.err != nil {
				continue
			}

			group := alignmentKey{action: ref.action}
			if majorVersionAlignment {
				major, ok := majorOfVersion(resolved.version)
				if !ok {
					continue
				}
				group.major = major
			}
			current, exists := alignedResolutions[group]
			if !exists || newerResolvedVersion(resolved, current) {
				alignedResolutions[group] = resolved
			}
		}
	}

	for _, wf := range workflows {
		wfLogger := logger.With("workflow", path.Base(wf.path))
		wfb := wf.content
		for _, job := range wf.Jobs {
			for _, step := range job.Steps {
				uses := strings.TrimSpace(step.Uses)
				if uses == "" {
					continue
				}
				if isTrustedAction(uses, trustedOrgs) {
					wfLogger.Debug("Skipping trusted action", "uses", uses)
					continue
				}
				if strings.HasPrefix(uses, "./") {
					wfLogger.Debug("Skipping local action", "uses", uses)
					continue
				}

				pkg, version, _ := strings.Cut(uses, "@")
				if len(version) == 40 {
					continue
				}

				ref := actionVersion{action: pkg, version: version}
				resolved := resolutions[ref]
				if resolved.err == errUnresolvedVersion {
					wfb = bytes.ReplaceAll(wfb, []byte(uses), []byte(fmt.Sprintf("%s@%s # TODO: use a release instead", pkg, version)))
					continue
				}
				if majorVersionAlignment || allVersionAlignment {
					group := alignmentKey{action: pkg}
					canAlign := true
					if majorVersionAlignment {
						if major, ok := majorOfVersion(resolved.version); ok {
							group.major = major
						} else {
							canAlign = false
						}
					}
					if canAlign {
						if aligned, ok := alignedResolutions[group]; ok {
							resolved = aligned
						}
					}
				}

				var newUses string
				if resolved.version == "master" || resolved.version == "main" {
					newUses = fmt.Sprintf("%s@%s # %s on %s, TODO: use a release instead", pkg, resolved.hash, resolved.version, today)
				} else if strings.Count(resolved.version, ".") < 2 {
					newUses = fmt.Sprintf("%s@%s # %s on %s, TODO: use a release instead", pkg, resolved.hash, resolved.version, today)
				} else {
					newUses = fmt.Sprintf("%s@%s # %s", pkg, resolved.hash, resolved.version)
				}

				wfb = bytes.ReplaceAll(wfb, []byte(uses), []byte(newUses))
			}
		}

		if err := afero.WriteFile(fs, wf.path, wfb, 0644); err != nil {
			return err
		}
	}

	return nil
}

type alignmentKey struct {
	action string
	major  string
}

func isTrustedAction(uses string, trustedOrgs []string) bool {
	for _, org := range trustedOrgs {
		if strings.HasPrefix(uses, org+"/") {
			return true
		}
	}
	return false
}

func majorOfVersion(version string) (string, bool) {
	numbers, ok := versionNumbers(version)
	if !ok {
		return "", false
	}
	return strconv.Itoa(numbers[0]), true
}

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

func newerResolvedVersion(candidate, current resolvedAction) bool {
	candidateNumbers, candidateIsVersion := versionNumbers(candidate.version)
	currentNumbers, currentIsVersion := versionNumbers(current.version)
	if candidateIsVersion != currentIsVersion {
		return candidateIsVersion
	}
	if candidateIsVersion {
		for i := range candidateNumbers {
			if candidateNumbers[i] != currentNumbers[i] {
				return candidateNumbers[i] > currentNumbers[i]
			}
		}
	}
	if candidate.version != current.version {
		return candidate.version < current.version
	}
	return candidate.hash < current.hash
}

const (
	forkRemoteName = "pin-gha_fork"
	prSignature    = "\n<!-- PR created with github.com/jcchavezs/pin-gha -->"
)

// patchRepository is the function that will be called for each repository by the iterator.
// It applies the patch and creates a PR if there are changes.
func patchRepository(ctx context.Context, logger *slog.Logger, repo string, isEmpty bool, xr iteratorexec.Execer, opts PatchOptions) error {
	rLogger := logger.With("repository", repo)

	if isEmpty {
		rLogger.Info("The repository is empty, skipping")
		opts.OnRepositorySkipped(ctx, repo, SkippedReasonEmptyRepository)
		return nil
	}

	if err := patchLocalRepositoryFSWithAlignment(ctx, rLogger, xr.GenerateFS(), opts.TrustedOrgs, opts.MajorVersionAlignment, opts.AllVersionAlignment); err != nil {
		return err
	}

	hasChanges, err := github.HasChanges(ctx, xr)
	if err != nil {
		return err
	} else if !hasChanges {
		rLogger.Info("The workflows are already pinned, skipping")
		opts.OnRepositorySkipped(ctx, repo, SkippedReasonAlreadyPinned)
		return nil
	}

	if err := github.CheckoutNewBranch(ctx, xr, opts.TargetBranch); err != nil {
		return err
	}

	if err := github.AddFiles(ctx, xr, ".github/workflows"); err != nil {
		return err
	}

	if err := github.Commit(ctx, xr, opts.CommitMsg, "--all"); err != nil {
		return err
	}

	head := opts.TargetBranch
	if opts.NeedsFork {
		logger.Debug("Forking repository and adding remote")

		if headFn, err := github.ForkAndAddRemote(ctx, xr, forkRemoteName); err != nil {
			return err
		} else {
			head = headFn(opts.TargetBranch)
		}

		err = github.PushToRemote(ctx, xr, forkRemoteName, opts.TargetBranch, github.PushForce)
	} else {
		err = github.Push(ctx, xr, opts.TargetBranch, github.PushForce)
	}
	if err != nil {
		return err
	}

	prURL, _, err := github.CreatePRIfNotExist(ctx, xr, github.PROptions{
		Title: opts.CommitMsg,
		Body:  opts.PRBody + prSignature,
		Head:  head,
		Draft: opts.PRAsDraft,
	})
	if err != nil {
		return err
	}

	logger.Info("PR created", "url", prURL)

	opts.OnPRCreated(ctx, repo, PRDetails{
		URL: prURL,
	})

	return nil
}

var errUnresolvedVersion = errors.New("unresolved version")

// resolveCommitHash is a package-level variable so tests can inject a mock
// without running real git commands.
var resolveCommitHash = ResolveCommitHashForAction

// ResolveCommitHashForAction resolves the commit hash for a specific GitHub Action version.
// Return values are:
// - the commit hash
// - the resolved version (which may be different from the input version if it was a tag or branch)
// - an error if the version could not be resolved.
func ResolveCommitHashForAction(ctx context.Context, action, version string) (string, string, error) {
	actionRepo := action
	if strings.Count(action, "/") > 1 {
		// if the action is not in the root of the repository the action name
		// will include the path but we only need the organization/repository name.
		pieces := strings.SplitN(action, "/", 3)
		actionRepo = strings.Join(pieces[0:2], "/")
	}

	var (
		hash            string
		resolvedVersion string
		err             error
	)
	err = iterator.RunForRepository(ctx, actionRepo, func(ctx context.Context, repository string, isEmpty bool, xr iteratorexec.Execer) error {
		if isEmpty {
			return errUnresolvedVersion
		}
		hash, resolvedVersion, err = resolveCommitHashForVersion(ctx, xr, version)
		return err
	}, iterator.Options{
		UseHTTPS:      true,
		CloningSubset: []string{"README.md"},
		LogHandler:    slog.Default().Handler(),
	})

	return hash, resolvedVersion, err
}

// resolveCommitHashForVersion contains the core git logic for resolving a version to a commit
// hash, given an already-cloned repository. Extracted so it can be tested without
// network access.
func resolveCommitHashForVersion(ctx context.Context, xr iteratorexec.Execer, version string) (string, string, error) {
	if _, err := xr.RunX(ctx, "git", "fetch", "--tags"); err != nil {
		return "", "", fmt.Errorf("fetching tags: %w", err)
	}

	// If the version looks like a full semver (i.e. has three dot-separated parts), we try directly
	if strings.Count(version, ".") == 2 {
		// First try to resolve the version as a branch or tag name. This covers the common cases of "master", "main", "v1", "v1.2", etc.
		return getLastReachableCommit(ctx, xr, version)
	}

	targetRef := version

	// If the version is not a branch or tag, try to find the most recent tag that starts with the version string. This covers cases where the version is something like "v1" or "v1.2" and the actual tags are "v1.0.0", "v1.1.0", "v1.2.0", etc.
	res, err := xr.Run(ctx, "git", "tag", "--list", fmt.Sprintf("%s*", version))
	if err != nil {
		return "", "", fmt.Errorf("listing tags: %w", err)
	} else {
		tags := strings.TrimSpace(res.Stdout)
		if res.ExitCode == 0 && tags != "" {
			xr.Log(ctx, slog.LevelDebug, "Found candidate versions", "versions", strings.Split(tags, "\n"))
			lastIndex := strings.LastIndex(tags, "\n")
			targetRef = tags[lastIndex+1:]
		}
	}

	// We try to resolve the version even if we didn't find any matching tags, just in case the version is a branch
	return getLastReachableCommit(ctx, xr, targetRef)
}

// getLastReachableCommit tries to resolve the given version as a branch or tag name and returns the corresponding commit hash.
func getLastReachableCommit(ctx context.Context, xr iteratorexec.Execer, version string) (string, string, error) {
	res, err := xr.Run(ctx, "git", "rev-list", "-n", "1", version)
	if err != nil {
		return "", "", fmt.Errorf("getting commit for tag: %w", err)
	}

	if res.ExitCode == 0 {
		return strings.TrimSpace(res.Stdout), version, nil
	}

	return "", "", errUnresolvedVersion
}
