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
	_, err := iterator.RunForOrganization(ctx, orgName, iterator.SearchOptions{
		ArchiveCondition: iterator.OmitArchived,
		SizeCondition:    iterator.NotEmpty,
	}, func(ctx context.Context, repo string, isEmpty bool, xr iteratorexec.Execer) error {
		return patchRepository(ctx, repo, isEmpty, xr, opts)
	}, iterator.Options{
		UseHTTPS:      true,
		LogHandler:    slog.Default().Handler(),
		CloningSubset: []string{".github/workflows"},
	})

	return err
}

// Repository processes a single repository.
func Repository(ctx context.Context, repo string, opts PatchOptions) error {
	opts = opts.withDefaults()
	if strings.HasPrefix(repo, ".") || strings.HasPrefix(repo, "/") {
		fs := afero.NewBasePathFs(afero.NewOsFs(), repo)
		return patchLocalRepositoryFS(ctx, fs, opts)
	}

	return iterator.RunForRepository(ctx, repo, func(ctx context.Context, repo string, isEmpty bool, xr iteratorexec.Execer) error {
		return patchRepository(ctx, repo, isEmpty, xr, opts)
	}, iterator.Options{
		UseHTTPS:      true,
		LogHandler:    slog.Default().Handler(),
		CloningSubset: []string{".github/workflows"},
	})
}

// patchLocalRepositoryFS applies the patch to the given FS. This is used for testing.
func patchLocalRepositoryFS(ctx context.Context, fs afero.Fs, opts PatchOptions) error {
	opts = opts.withDefaults()
	today := time.Now().Format("2006-01-02")
	files, err := afero.ReadDir(fs, path.Join(".github", "workflows"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

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
			return err
		}

		for _, job := range wf.Jobs {
			for _, step := range job.Steps {
				uses := strings.TrimSpace(step.Uses)
				if uses == "" {
					continue
				}

				isTrusted := false
				for _, org := range opts.TrustedOrgs {
					if strings.HasPrefix(uses, org+"/") {
						isTrusted = true
						break
					}
				}
				if isTrusted {
					continue
				}

				if strings.HasPrefix(uses, "./") {
					//if exists, _ := afero.Exists(fs, uses); exists {
					// local action
					continue
					//}
				}

				pkg, version, _ := strings.Cut(uses, "@")

				if version == "" {
					return fmt.Errorf("no version specified for action %s in %q", pkg, file.Name())
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
						newUses = fmt.Sprintf("%s@%s # %s on %s, TODO: consider using a release", pkg, resolvedHash, resolvedVersion, today)
					} else {
						newUses = fmt.Sprintf("%s@%s # %s", pkg, resolvedHash, resolvedVersion)
					}
				case errUnresolvedVersion:
					newUses = fmt.Sprintf("%s@%s # TODO: use a release instead", pkg, version)
				default:
					return fmt.Errorf("getting commit hash: %v", err)
				}

				wfb = bytes.ReplaceAll(wfb, []byte(uses), []byte(newUses))
			}
		}

		err = afero.WriteFile(fs, workflowPath, wfb, 0644)
		if err != nil {
			return err
		}
	}

	return nil
}

// patchRepository is the function that will be called for each repository by the iterator.
// It applies the patch and creates a PR if there are changes.
func patchRepository(ctx context.Context, _ string, isEmpty bool, xr iteratorexec.Execer, opts PatchOptions) error {
	if isEmpty {
		return nil
	}

	opts = opts.withDefaults()

	if err := patchLocalRepositoryFS(ctx, xr.GenerateFS(), opts); err != nil {
		return err
	}

	hasChanges, err := github.HasChanges(ctx, xr)
	if err != nil {
		return err
	} else if !hasChanges {
		return nil
	}

	if err := github.CheckoutNewBranch(ctx, xr, opts.TargetBranch); err != nil {
		return err
	}

	if err := github.AddFiles(ctx, xr, ".github/workflows"); err != nil {
		return err
	}

	if err := github.Commit(ctx, xr, opts.CommitMsg); err != nil {
		return err
	}

	if err := github.Push(ctx, xr, opts.TargetBranch, github.PushForce); err != nil {
		return err
	}

	prURL, _, err := github.CreatePRIfNotExist(ctx, xr, github.PROptions{
		Title: opts.CommitMsg,
		Body:  opts.PRBody,
		Head:  opts.TargetBranch,
	})
	if err != nil {
		return err
	}

	xr.Log(ctx, slog.LevelDebug, "PR created", "pr_url", prURL)

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
