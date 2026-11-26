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
		Name  string `yaml:"name"`
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

// patchFS pins the actions used in the workflow files and, if includeActions is
// set, in the action definition files too.
func patchFS(ctx context.Context, logger *slog.Logger, fs afero.Fs, trustedOrgs []string, includeActions bool) ([]string, error) {
	var changed []string
	today := time.Now().Format("2006-01-02")
	var filePaths []string
	files, err := afero.ReadDir(fs, path.Join(".github", "workflows"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	if len(files) == 0 {
		logger.Debug("No files found")
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

		type step = struct {
			Name string `yaml:"name"`
			Uses string `yaml:"uses"`
		}
		var allSteps []step
		for _, job := range wf.Jobs {
			allSteps = append(allSteps, job.Steps...)
		}
		allSteps = append(allSteps, wf.Runs.Steps...)

		for _, step := range allSteps {
			uses := strings.TrimSpace(step.Uses)
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

			if version == "" {
				return changed, fmt.Errorf("no version specified for action %s in %q", pkg, workflowPath)
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

	if _, err := patchFS(ctx, rLogger, xr.GenerateFS(), opts.TrustedOrgs, opts.IncludeActionDefinitions); err != nil {
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
