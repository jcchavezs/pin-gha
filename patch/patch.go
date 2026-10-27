package patch

import (
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
	opts = opts.withDefaults()

	return iterator.RunForRepository(ctx, normalizeRepositoryName(repo), func(ctx context.Context, repo string, isEmpty bool, xr iteratorexec.Execer) error {
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

func normalizeRepositoryName(repo string) string {
	return strings.TrimPrefix(repo, "https://github.com/")
}

// LocalRepository processes a local repository at the given path. This is used for testing and can also be used by users who want to apply the patch to a local repository without creating a PR.
func LocalRepository(ctx context.Context, repoPath string, opts LocalPatchOptions) error {
	opts = opts.withDefaults()
	return patchLocalRepositoryFS(ctx, slog.New(opts.LogHandler), afero.NewBasePathFs(afero.NewOsFs(), repoPath), opts.TrustedOrgs, opts.VersionAlignment)
}

// patchLocalRepositoryFS applies the patch to the given FS. This is used for testing.
func patchLocalRepositoryFS(ctx context.Context, logger *slog.Logger, fs afero.Fs, trustedOrgs []string, alignment VersionAlignment) error {
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

	// Every workflow is read and every action resolved before any file is
	// written, so an error leaves the repository untouched and the versions
	// can be aligned across files.
	var workflows []workflowFile
	for _, file := range files {
		if file.IsDir() || !isWorkflowFile(file.Name()) {
			continue
		}

		wf, err := readWorkflow(logger, fs, file.Name(), trustedOrgs)
		if err != nil {
			return err
		}
		workflows = append(workflows, wf)
	}

	resolutions, refs, err := resolveActions(ctx, workflows)
	if err != nil {
		return err
	}
	highest := highestVersions(refs, resolutions, alignment)

	today := time.Now().Format("2006-01-02")
	for _, wf := range workflows {
		wfLogger := logger.With("workflow", path.Base(wf.path))
		content := wf.content
		for _, use := range wf.uses {
			resolved := resolutions[actionVersion{action: use.action, version: use.version}]
			if errors.Is(resolved.err, errUnresolvedVersion) {
				content = replaceUses(content, use.raw, fmt.Sprintf("%s@%s # TODO: use a release instead", use.action, use.version))
				continue
			}

			resolved = alignedResolution(wfLogger, use.action, resolved, highest, alignment)
			content = replaceUses(content, use.raw, pinnedUses(use.action, resolved, today))
		}

		if err := afero.WriteFile(fs, wf.path, content, 0644); err != nil {
			return err
		}
	}

	return nil
}

// actionUse is a third-party action used by a workflow step that has to be pinned.
type actionUse struct {
	// raw is the value of uses as written in the workflow.
	raw     string
	action  string
	version string
}

type workflowFile struct {
	path    string
	content []byte
	// uses are the steps to pin, in the order they appear in the workflow.
	uses []actionUse
}

func isWorkflowFile(name string) bool {
	return strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml")
}

func readWorkflow(logger *slog.Logger, fs afero.Fs, name string, trustedOrgs []string) (workflowFile, error) {
	workflowPath := path.Join(".github", "workflows", name)
	content, err := afero.ReadFile(fs, workflowPath)
	if err != nil {
		return workflowFile{}, err
	}

	wf := workflow{}
	if err := yaml.Unmarshal(content, &wf); err != nil {
		return workflowFile{}, fmt.Errorf("unmarshalling workflow %q: %v", name, err)
	}

	wfLogger := logger.With("workflow", name)
	var uses []actionUse
	for _, job := range wf.Jobs {
		for _, step := range job.Steps {
			raw := strings.TrimSpace(step.Uses)
			if raw == "" {
				continue
			}

			if isTrustedAction(raw, trustedOrgs) {
				wfLogger.Debug("Skipping trusted action", "uses", raw)
				continue
			}

			if strings.HasPrefix(raw, "./") {
				wfLogger.Debug("Skipping local action", "uses", raw)
				continue
			}

			action, version, _ := strings.Cut(raw, "@")
			if version == "" {
				return workflowFile{}, fmt.Errorf("no version specified for action %s in %q", action, name)
			}

			if len(version) == 40 { // commit hash length
				continue
			}

			uses = append(uses, actionUse{raw: raw, action: action, version: version})
		}
	}

	return workflowFile{path: workflowPath, content: content, uses: uses}, nil
}

// resolveActions resolves every distinct action version once. refs holds them
// in the order they were found.
func resolveActions(ctx context.Context, workflows []workflowFile) (resolutions map[actionVersion]resolvedAction, refs []actionVersion, err error) {
	resolutions = make(map[actionVersion]resolvedAction)
	for _, wf := range workflows {
		for _, use := range wf.uses {
			ref := actionVersion{action: use.action, version: use.version}
			if _, exists := resolutions[ref]; exists {
				continue
			}

			hash, version, err := resolveCommitHash(ctx, use.action, use.version)
			if err != nil && !errors.Is(err, errUnresolvedVersion) {
				return nil, nil, fmt.Errorf("resolving %s@%s: %w", use.action, use.version, err)
			}
			resolutions[ref] = resolvedAction{hash: hash, version: version, err: err}
			refs = append(refs, ref)
		}
	}

	return resolutions, refs, nil
}

func isTrustedAction(uses string, trustedOrgs []string) bool {
	for _, org := range trustedOrgs {
		if strings.HasPrefix(uses, org+"/") {
			return true
		}
	}
	return false
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
func patchRepository(ctx context.Context, logger *slog.Logger, repo string, isEmpty bool, xr iteratorexec.Execer, opts PatchOptions) error {
	rLogger := logger.With("repository", repo)

	if isEmpty {
		rLogger.Info("The repository is empty, skipping")
		opts.OnRepositorySkipped(ctx, repo, SkippedReasonEmptyRepository)
		return nil
	}

	if err := patchLocalRepositoryFS(ctx, rLogger, xr.GenerateFS(), opts.TrustedOrgs, opts.VersionAlignment); err != nil {
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
