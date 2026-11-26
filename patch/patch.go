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
	"regexp"
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
		Name string `yaml:"name"`
		// Uses is only present in jobs calling a reusable workflow.
		Uses  string `yaml:"uses"`
		Steps []struct {
			Name string `yaml:"name"`
			Uses string `yaml:"uses"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
	// Runs is only present in action definitions (action.yml/action.yaml).
	Runs struct {
		Steps []struct {
			Name string `yaml:"name"`
			Uses string `yaml:"uses"`
		} `yaml:"steps"`
	} `yaml:"runs"`
}

// requiredCommands are the external commands needed to patch remote repositories.
var requiredCommands = []string{"git", "gh"}

// checkRequiredCommands returns an error if any of the required commands is not in PATH.
func checkRequiredCommands() error {
	for _, c := range requiredCommands {
		if err := checkCommandExist(c); err != nil {
			return err
		}
	}
	return nil
}

func checkCommandExist(name string) error {
	if _, err := exec.LookPath(name); err != nil {
		return fmt.Errorf("failed to find '%s' command, please install it and try again: %w", name, err)
	}
	return nil
}

// Organization processes all repositories in the given organization.
func Organization(ctx context.Context, orgName string, opts PatchOptions) error {
	if err := checkRequiredCommands(); err != nil {
		return err
	}

	opts = opts.withDefaults()

	_, err := iterator.RunForOrganization(ctx, orgName, iterator.SearchOptions{
		ArchiveCondition: iterator.OmitArchived,
		SizeCondition:    iterator.NotEmpty,
		Source:           iterator.OnlyNonForks,
		Page:             iterator.AllPages,
	}, func(ctx context.Context, repository iterator.Repository, xr iteratorexec.Execer) error {
		if err := patchRepository(ctx, slog.New(opts.LogHandler), repository, xr, opts); err != nil {
			return opts.OnRepositoryErr(ctx, repository.Name, err)
		}

		return nil
	}, iterator.Options{
		UseHTTPS:      true,
		LogHandler:    opts.LogHandler,
		CloningSubset: cloningSubset(opts),
	})

	return err
}

// Repository processes a single repository.
func Repository(ctx context.Context, repo string, opts PatchOptions) error {
	if err := checkRequiredCommands(); err != nil {
		return err
	}

	opts = opts.withDefaults()

	return iterator.RunForRepository(ctx, normalizeRepositoryName(repo), func(ctx context.Context, repository iterator.Repository, xr iteratorexec.Execer) error {
		if err := patchRepository(ctx, slog.New(opts.LogHandler), repository, xr, opts); err != nil {
			return opts.OnRepositoryErr(ctx, repository.Name, err)
		}

		return nil
	}, iterator.Options{
		UseHTTPS:      true,
		LogHandler:    opts.LogHandler,
		CloningSubset: cloningSubset(opts),
	})
}

// cloningSubset returns the paths that need to be cloned to patch a repository.
func cloningSubset(opts PatchOptions) []string {
	subset := []string{".github/workflows"}
	if opts.IncludeActionDefinitions {
		subset = append(subset, ".github/actions", "action.yml", "action.yaml")
	}
	return subset
}

func normalizeRepositoryName(repo string) string {
	return strings.TrimPrefix(repo, "https://github.com/")
}

// LocalRepository processes a local repository at the given path. This is used for testing and can also be used by users who want to apply the patch to a local repository without creating a PR.
func LocalRepository(ctx context.Context, repoPath string, opts LocalPatchOptions) error {
	opts = opts.withDefaults()
	changed, err := patchFS(ctx, slog.New(opts.LogHandler), afero.NewBasePathFs(afero.NewOsFs(), repoPath), opts.TrustedOrgs, opts.IncludeActionDefinitions)
	for _, f := range changed {
		opts.OnFileChanged(ctx, f)
	}
	return err
}

// patchLocalRepositoryFS applies the patch to the workflow files of the given
// FS and returns the paths of the files whose content changed. This is used for
// testing.
func patchLocalRepositoryFS(ctx context.Context, logger *slog.Logger, fs afero.Fs, trustedOrgs []string) ([]string, error) {
	return patchFS(ctx, logger, fs, trustedOrgs, false)
}

// actionDefinitionPaths returns the action definition files (action.yml and
// action.yaml) in the root of the repository and in .github/actions/*.
func actionDefinitionPaths(fs afero.Fs) []string {
	dirs := []string{"."}
	if entries, err := afero.ReadDir(fs, path.Join(".github", "actions")); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				dirs = append(dirs, path.Join(".github", "actions", e.Name()))
			}
		}
	}

	var paths []string
	for _, dir := range dirs {
		for _, name := range []string{"action.yml", "action.yaml"} {
			p := path.Join(dir, name)
			if ok, _ := afero.Exists(fs, p); ok {
				paths = append(paths, p)
			}
		}
	}
	return paths
}

// pinnableFiles returns the workflow files and, if includeActions is set, the action
// definition files of the FS.
func pinnableFiles(fs afero.Fs, includeActions bool) ([]string, error) {
	var filePaths []string
	files, err := afero.ReadDir(fs, path.Join(".github", "workflows"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	for _, file := range files {
		if file.IsDir() {
			continue
		}

		if !strings.HasSuffix(file.Name(), ".yml") && !strings.HasSuffix(file.Name(), ".yaml") {
			continue
		}

		filePaths = append(filePaths, path.Join(".github", "workflows", file.Name()))
	}

	if includeActions {
		filePaths = append(filePaths, actionDefinitionPaths(fs)...)
	}

	return filePaths, nil
}

// patchFS pins the actions used in the workflow files and, if includeActions is
// set, in the action definition files too.
func patchFS(ctx context.Context, logger *slog.Logger, fs afero.Fs, trustedOrgs []string, includeActions bool) ([]string, error) {
	var changed []string
	today := time.Now().Format("2006-01-02")
	filePaths, err := pinnableFiles(fs, includeActions)
	if err != nil {
		return nil, err
	}

	if len(filePaths) == 0 {
		logger.Debug("No files found")
	}

	for _, workflowPath := range filePaths {
		original, err := afero.ReadFile(fs, workflowPath)
		if err != nil {
			return changed, err
		}
		wfb := original

		wfLogger := logger.With("workflow", workflowPath)
		wf := workflow{}
		if err := yaml.Unmarshal(wfb, &wf); err != nil {
			return changed, fmt.Errorf("unmarshalling workflow %q: %v", workflowPath, err)
		}

		var allUses []string
		for _, job := range wf.Jobs {
			allUses = append(allUses, job.Uses)
			for _, step := range job.Steps {
				allUses = append(allUses, step.Uses)
			}
		}
		for _, step := range wf.Runs.Steps {
			allUses = append(allUses, step.Uses)
		}

		for _, use := range allUses {
			uses := strings.TrimSpace(use)
			if uses == "" {
				continue
			}

			isTrusted := false
			for _, org := range trustedOrgs {
				if strings.HasPrefix(uses, org+"/") {
					isTrusted = true
					break
				}
			}
			if isTrusted {
				wfLogger.Debug("Skipping trusted action", "uses", uses)
				continue
			}

			if strings.HasPrefix(uses, "./") {
				wfLogger.Debug("Skipping local action", "uses", uses)
				//if exists, _ := afero.Exists(fs, uses); exists {
				// local action
				continue
				//}
			}

			// Docker image actions are not pinned yet: https://github.com/jcchavezs/pin-gha/issues/29
			if strings.HasPrefix(uses, "docker://") {
				wfLogger.Debug("Skipping docker action", "uses", uses)
				continue
			}

			pkg, version, _ := strings.Cut(uses, "@")

			// A reference we cannot interpret should not prevent pinning the rest of the repository.
			if !actionRefRe.MatchString(pkg) {
				wfLogger.Warn("Skipping unparsable uses reference", "uses", uses)
				continue
			}

			if version == "" {
				// GitHub runs the default branch when no version is given, so we pin its latest commit.
				wfLogger.Warn("No version specified for action, pinning the latest commit of the default branch", "uses", uses)
			}

			if len(version) == 40 { // commit hash length
				continue
			}

			var newUses string
			resolvedHash, resolvedVersion, err := resolveCommitHash(ctx, pkg, version)
			switch err {
			case nil:
				if resolvedVersion == "master" || resolvedVersion == "main" {
					newUses = fmt.Sprintf("%s@%s # %s on %s, TODO: use a release instead", pkg, resolvedHash, resolvedVersion, today)
				} else if strings.Count(resolvedVersion, ".") < 2 {
					newUses = fmt.Sprintf("%s@%s # %s on %s, TODO: use a release instead", pkg, resolvedHash, resolvedVersion, today)
				} else {
					newUses = fmt.Sprintf("%s@%s # %s", pkg, resolvedHash, resolvedVersion)
				}
			case errUnresolvedVersion:
				newUses = fmt.Sprintf("%s@%s # TODO: use a release instead", pkg, version)
			default:
				return changed, fmt.Errorf("getting commit hash: %v", err)
			}

			wfb = replaceUses(wfb, uses, newUses)
		}

		if bytes.Equal(original, wfb) {
			continue
		}

		if err := afero.WriteFile(fs, workflowPath, wfb, 0644); err != nil {
			return changed, err
		}
		changed = append(changed, workflowPath)
	}

	return changed, nil
}

// replaceUses replaces only the occurrences of uses that are a whole value, so
// "actions/checkout@v4" does not match inside "actions/checkout@v4.1.0" or
// "other-actions/checkout@v4".
func replaceUses(content []byte, uses, newUses string) []byte {
	re := regexp.MustCompile(`(^|[\s"'])` + regexp.QuoteMeta(uses) + `($|[\s"'])`)
	// The comment goes after the closing quote, otherwise it becomes part of the value.
	value, comment, hasComment := strings.Cut(newUses, " # ")
	return re.ReplaceAllFunc(content, func(match []byte) []byte {
		sub := re.FindSubmatch(match)
		lead, trail := sub[1], sub[2]
		if hasComment && (string(trail) == `"` || string(trail) == "'") {
			return []byte(string(lead) + value + string(trail) + " # " + comment)
		}
		return []byte(string(lead) + newUses + string(trail))
	})
}

const (
	forkRemoteName = "pin-gha_fork"
	prSignature    = "\n<!-- PR created with github.com/jcchavezs/pin-gha -->"
)

// patchRepository is the function that will be called for each repository by the iterator.
// It applies the patch and creates a PR if there are changes.
func patchRepository(ctx context.Context, logger *slog.Logger, repository iterator.Repository, xr iteratorexec.Execer, opts PatchOptions) error {
	repo := repository.Name
	rLogger := logger.With("repository", repo)

	if repository.IsEmpty() {
		rLogger.Info("The repository is empty, skipping")
		opts.OnRepositorySkipped(ctx, repo, SkippedReasonEmptyRepository)
		return nil
	}

	fs := xr.GenerateFS()
	if files, err := pinnableFiles(fs, opts.IncludeActionDefinitions); err != nil {
		return err
	} else if len(files) == 0 {
		rLogger.Info("The repository has no workflows, skipping")
		opts.OnRepositorySkipped(ctx, repo, SkippedReasonNoWorkflows)
		return nil
	}

	if _, err := patchFS(ctx, rLogger, fs, opts.TrustedOrgs, opts.IncludeActionDefinitions); err != nil {
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

	addPaths := []string{".github/workflows"}
	if opts.IncludeActionDefinitions {
		addPaths = cloningSubset(opts)
	}
	for _, addPath := range addPaths {
		// git add fails on a pathspec that does not exist, e.g. a repository without .github/actions.
		if exists, _ := afero.Exists(fs, addPath); !exists {
			continue
		}

		if err := github.AddFiles(ctx, xr, addPath); err != nil {
			return err
		}
	}

	if opts.DryRun {
		details, err := stagedChanges(ctx, xr, opts.TargetBranch)
		if err != nil {
			return err
		}

		rLogger.Info("dry-run: skipping push and PR creation", "files", details.Files)
		opts.OnDryRunChanges(ctx, repo, details)
		return nil
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

func stagedChanges(ctx context.Context, xr iteratorexec.Execer, branch string) (DryRunDetails, error) {
	names, err := xr.RunX(ctx, "git", "diff", "--cached", "--name-only")
	if err != nil {
		return DryRunDetails{}, fmt.Errorf("listing staged files: %w", err)
	}

	diff, err := xr.RunX(ctx, "git", "diff", "--cached")
	if err != nil {
		return DryRunDetails{}, fmt.Errorf("getting staged diff: %w", err)
	}

	return DryRunDetails{
		Branch: branch,
		Diff:   diff,
		Files:  strings.Fields(names),
	}, nil
}

// actionRefRe matches the owner/repo[/path] shape of a remote action or reusable workflow.
var actionRefRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+(/.+)?$`)

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
	err = iterator.RunForRepository(ctx, actionRepo, func(ctx context.Context, repository iterator.Repository, xr iteratorexec.Execer) error {
		hash, resolvedVersion, err = resolveCommitHashInRepository(ctx, repository, xr, version)
		return err
	}, iterator.Options{
		UseHTTPS:      true,
		CloningSubset: []string{"README.md"},
		LogHandler:    slog.Default().Handler(),
	})

	return hash, resolvedVersion, err
}

// resolveCommitHashInRepository resolves the version in the given repository, failing
// with errUnresolvedVersion if the repository is empty.
func resolveCommitHashInRepository(ctx context.Context, repository iterator.Repository, xr iteratorexec.Execer, version string) (string, string, error) {
	if repository.IsEmpty() {
		return "", "", errUnresolvedVersion
	}

	return resolveCommitHashForVersion(ctx, xr, version)
}

// resolveCommitHashForVersion contains the core git logic for resolving a version to a commit
// hash, given an already-cloned repository. Extracted so it can be tested without
// network access.
func resolveCommitHashForVersion(ctx context.Context, xr iteratorexec.Execer, version string) (string, string, error) {
	if _, err := xr.RunX(ctx, "git", "fetch", "--tags"); err != nil {
		return "", "", fmt.Errorf("fetching tags: %w", err)
	}

	// Without a version GitHub runs the default branch, so we resolve its latest commit.
	if version == "" {
		hash, _, err := getLastReachableCommit(ctx, xr, "HEAD")
		if err != nil {
			return "", "", err
		}

		branch, err := xr.RunX(ctx, "git", "rev-parse", "--abbrev-ref", "HEAD")
		if err != nil {
			return "", "", fmt.Errorf("getting default branch: %w", err)
		}

		return hash, strings.TrimSpace(branch), nil
	}

	// If the version looks like a full semver (i.e. has three dot-separated parts), we try directly
	if strings.Count(version, ".") == 2 {
		// First try to resolve the version as a branch or tag name. This covers the common cases of "master", "main", "v1", "v1.2", etc.
		return getLastReachableCommit(ctx, xr, version)
	}

	// Floating refs such as "v4" or "v1.2" usually exist as a tag or a branch, in which case we
	// keep the commit they point to today instead of guessing which release tag is the latest.
	for _, ref := range []string{"refs/tags/" + version, version} {
		hash, _, err := getLastReachableCommit(ctx, xr, ref)
		if errors.Is(err, errUnresolvedVersion) {
			continue
		} else if err != nil {
			return "", "", err
		}

		resolvedVersion, err := releaseTagAt(ctx, xr, hash, version)
		return hash, resolvedVersion, err
	}

	// Otherwise, we look for the highest release tag that starts with the version, e.g. "v1.2.0",
	// "v1.3.0" for "v1".
	tag, err := latestReleaseTag(ctx, xr, "", version)
	if err != nil {
		return "", "", err
	} else if tag == "" {
		return "", "", errUnresolvedVersion
	}

	hash, _, err := getLastReachableCommit(ctx, xr, "refs/tags/"+tag)
	return hash, tag, err
}

// releaseTagRe matches the release tags we pin to, that is full semver tags without pre-release
// or build metadata.
var releaseTagRe = regexp.MustCompile(`^v?\d+\.\d+\.\d+$`)

// releaseTagAt returns the highest release tag under the version prefix pointing at the commit,
// or the version itself if the commit has none.
func releaseTagAt(ctx context.Context, xr iteratorexec.Execer, hash, version string) (string, error) {
	tag, err := latestReleaseTag(ctx, xr, hash, version)
	if err != nil {
		return "", err
	} else if tag == "" {
		return version, nil
	}

	return tag, nil
}

// latestReleaseTag returns the highest release tag under the version prefix (e.g. "v1.*" for
// "v1"), optionally limited to the tags pointing at the commit. It returns an empty string if
// there is none.
func latestReleaseTag(ctx context.Context, xr iteratorexec.Execer, hash, version string) (string, error) {
	// The dot anchors the prefix so "v1" does not match "v10.0.0" or "v1-beta".
	args := []string{"tag", "--list", "--sort=-version:refname", version + ".*"}
	if hash != "" {
		args = append(args, "--points-at", hash)
	}

	res, err := xr.Run(ctx, "git", args...)
	if err != nil {
		return "", fmt.Errorf("listing tags: %w", err)
	} else if res.ExitCode != 0 {
		return "", nil
	}

	tags := strings.Fields(res.Stdout)
	xr.Log(ctx, slog.LevelDebug, "Found candidate versions", "versions", tags)
	for _, tag := range tags {
		if releaseTagRe.MatchString(tag) {
			return tag, nil
		}
	}

	return "", nil
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
