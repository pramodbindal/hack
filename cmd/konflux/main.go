package main

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"

	k "github.com/openshift-pipelines/hack/internal/konflux"
	// "golang.org/x/sync/errgroup"
	"gopkg.in/yaml.v2"
)

var nameFieldInvalidCharPattern = regexp.MustCompile("[^a-z0-9]")

//go:embed templates/konflux/*.yaml templates/github/*/*.yaml templates/tekton/*.yaml
var templateFS embed.FS

func main() {
	ctx := context.Background()

	dryRun := flag.Bool("dry-run", false, "Dry run (no commit, …)")
	tmpDir := flag.String("dir", "", "folder to work in. If empty, will create a temporary one")
	flag.Parse()

	if _, err := exec.LookPath("gh"); !*dryRun && err != nil {
		log.Fatal("Couldn't find gh in your path, bailing.")
	}
	if _, err := exec.LookPath("jq"); !*dryRun && err != nil {
		log.Fatal("Couldn't find jq in your path, bailing.")
	}

	configs := flag.Args()

	dir := *tmpDir
	if dir == "" {
		var err error
		dir, err = os.MkdirTemp("", "update-konflux-repo")
		if err != nil {
			log.Fatal(err)
		}
	}

	for _, config := range configs {
		in, err := os.ReadFile(config)
		if err != nil {
			log.Fatal(err)
		}
		c := &k.Config{}
		if err := yaml.UnmarshalStrict(in, c); err != nil {
			log.Fatal(err)
		}

		fmt.Printf("::group:: generating konflux configuration for %s\n", c.Repository)
		if err := generateConfig(ctx, *c, dir, *dryRun); err != nil {
			log.Fatal(err)
		}
		//if err := generateBranchesConfig(ctx, *c, dir, *dryRun); err != nil {
		//	log.Fatal(err)
		//}
		fmt.Println("::endgroup::")
	}
}

const GithubOrg = "openshift-pipelines"

func generateConfig(ctx context.Context, c k.Config, dir string, dryRun bool) error {
	repo := fmt.Sprintf("https://github.com/%s/%s.git", GithubOrg, c.Repository)
	fmt.Printf("::group:: generating konflux configuration for %s\n", repo)

	if len(c.Branches) == 0 {
		c.Branches = []k.Branch{{
			Name: "main",
		}}
	}
	for _, branch := range c.Branches {
		checkoutDir := filepath.Join(dir, c.Repository+"-"+branch.Name)
		log.Printf("Generating %s (%s) on %s in %s\n", c.Repository, repo, branch.Name, checkoutDir)

		if err := os.MkdirAll(checkoutDir, os.ModePerm); err != nil {
			return err
		}
		if err := cloneAndCheckout(ctx, repo, branch.Name, checkoutDir); err != nil {
			return err
		}
		// cleanup stuff
		if err := cleanupAutogenerated(ctx, checkoutDir); err != nil {
			return err
		}
		if len(branch.Versions) == 0 {
			branch.Versions = []k.Version{{
				Version: branch.Name,
			}}
		}
		if c.Upstream != "" {
			app := k.Application{
				Upstream:       c.Upstream,
				Branch:         branch.Name,
				GitHub:         c.GitHub,
				UpstreamBranch: branch.UpstreamBranch,
			}
			if err := generateGitHub(app, filepath.Join(checkoutDir, ".github")); err != nil {
				log.Fatalln(err)
			}
		}

		for _, v := range branch.Versions {
			app := k.Application{
				Name:        c.Repository,
				Repository:  path.Join(GithubOrg, c.Repository),
				Upstream:    c.Upstream,
				Components:  c.Components,
				Branch:      branch.Name,
				Version:     v.Version,
				GitHub:      c.GitHub,
				Tekton:      c.Tekton,
				Patches:     c.Patches,
				ReleasePlan: v.Release == "auto",
				Platforms:   mainPlatforms(c.Platforms),
			}
			if err := generateKonflux(app, filepath.Join(checkoutDir, ".konflux")); err != nil {
				log.Fatalln(err)
			}
			if err := generateTekton(app, filepath.Join(checkoutDir, ".tekton")); err != nil {
				log.Fatalln(err)
			}
			if !dryRun {
				if err := commitAndPullRequest(ctx, checkoutDir, branch.Name); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func commitAndPullRequest(ctx context.Context, dir, branch string) error {
	if out, err := run(ctx, dir, "git", "status", "--porcelain"); err != nil {
		return fmt.Errorf("failed to check git status: %s, %s", err, out)
	} else if string(out) == "" {
		log.Printf("[%s] No changes, skipping commit and PR", dir)
		return nil
	}
	if out, err := run(ctx, dir, "bash", "-c", "git config user.name openshift-pipelines-bot; git config user.email pipelines-extcomm@redhat.com"); err != nil {
		return fmt.Errorf("failed to set some git configurations: %s, %s", err, out)
	}
	if out, err := run(ctx, dir, "git", "add", "."); err != nil {
		return fmt.Errorf("failed to add: %s, %s", err, out)
	}
	if out, err := run(ctx, dir, "git", "commit", "-m", fmt.Sprintf("[bot:%s] update konflux configuration", branch)); err != nil {
		return fmt.Errorf("failed to commit: %s, %s", err, out)
	}
	if out, err := run(ctx, dir, "git", "push", "-f", "origin", "actions/update/konflux-configuration-"+branch); err != nil {
		return fmt.Errorf("failed to push: %s, %s", err, out)
	}
	if out, err := run(ctx, dir, "bash", "-c", "gh pr list --base "+branch+" --head actions/update/konflux-configuration-"+branch+" --json url --jq 'length'"); err != nil {
		return fmt.Errorf("failed to check if a pr exists: %s, %s", err, out)
	} else if strings.TrimSpace(string(out)) == "0" {
		if out, err := run(ctx, dir, "gh", "pr", "create", "-d",
			"--base", branch,
			"--head", "actions/update/konflux-configuration-"+branch,
			"--label=hack", "--label=automated",
			"--title", fmt.Sprintf("[bot:%s] update konflux configuration", branch),
			"--body", "This PR was automatically generated by the konflux command from openshift-pipelines/hack repository"); err != nil {
			return fmt.Errorf("failed to create the pr: %s, %s", err, out)
		}
	}
	return nil
}

func cloneAndCheckout(ctx context.Context, repo, branch, dir string) error {
	exists, err := exists(filepath.Join(dir, ".git"))
	if err != nil {
		return err
	}
	if exists {
		// Repository exists, fetch the latest changes
		if out, err := run(ctx, dir, "git", "fetch", "--all"); err != nil {
			return fmt.Errorf("failed to fetch repository: %s, %s", err, out)
		}
	} else {
		// Repository does not exists, clone the repository
		if out, err := run(ctx, dir, "git", "clone", repo, "."); err != nil {
			return fmt.Errorf("failed to clone repository: %s, %s", err, out)
		}
	}
	if out, err := run(ctx, dir, "git", "reset", "--hard", "HEAD", "--"); err != nil {
		return fmt.Errorf("failed to reset %s branch: %s, %s", branch, err, out)
	}
	if out, err := run(ctx, dir, "git", "checkout", "origin/"+branch, "-B", branch); err != nil {
		return fmt.Errorf("failed to checkout %s branch: %s, %s", branch, err, out)
	}
	if out, err := run(ctx, dir, "git", "checkout", "-B", "actions/update/konflux-configuration-"+branch); err != nil {
		return fmt.Errorf("failed to checkout branch for PR: %s, %s", err, out)
	}
	return nil
}

func mainPlatforms(platforms []string) []string {
	if len(platforms) == 0 {
		return []string{"linux/x86_64", "linux-m2xlarge/arm64"}
	}
	return platforms
}

func generateTekton(application k.Application, target string) error {
	log.Printf("Generate tekton manifest in %s\n", target)
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}

	// set defaults
	if application.Tekton.WatchedSources == "" {
		application.Tekton.WatchedSources = `"upstream/***".pathChanged() || ".konflux/patches/***".pathChanged() || ".konflux/rpms/***".pathChanged()`
	}

	for _, c := range application.Components {
		updateComponent(application, &c)
		switch application.Tekton.EventType {
		case "pull_request":
			if err := generateFileFromTemplate("component-pull-request.yaml", c, filepath.Join(target, fmt.Sprintf("%s-%s-%s-pull-request.yaml", hyphenize(basename(application.Repository)), hyphenize(application.Version), c.Name))); err != nil {
				return err
			}
		case "push":
			if err := generateFileFromTemplate("component-push.yaml", c, filepath.Join(target, fmt.Sprintf("%s-%s-%s-push.yaml", hyphenize(basename(application.Repository)), hyphenize(application.Version), c.Name))); err != nil {
				return err
			}
		default:
			if err := generateFileFromTemplate("component-pull-request.yaml", c, filepath.Join(target, fmt.Sprintf("%s-%s-%s-pull-request.yaml", hyphenize(basename(application.Repository)), hyphenize(application.Version), c.Name))); err != nil {
				return err
			}
			if err := generateFileFromTemplate("component-push.yaml", c, filepath.Join(target, fmt.Sprintf("%s-%s-%s-push.yaml", hyphenize(basename(application.Repository)), hyphenize(application.Version), c.Name))); err != nil {
				return err
			}
		}
	}
	return nil
}

// This function can modified in future if we want to override the fields at component level.
func updateComponent(application k.Application, c *k.Component) {
	c.Application = application.Name
	c.Repository = application.Repository
	c.Branch = application.Branch
	c.Version = application.Version
	c.Platforms = application.Platforms
	if c.Tekton == (k.Tekton{}) {
		c.Tekton = application.Tekton
	}
	if c.Dockerfile == "" {
		c.Dockerfile, _ = eval(".konflux/dockerfiles/{{.Name}}.Dockerfile", c)
	}
	if c.PrefetchInput == "" {
		c.PrefetchInput = "{\"type\": \"rpm\", \"path\": \".konflux/rpms\"}"
	}
}

func generateKonflux(application k.Application, target string) error {
	log.Printf("Generate konflux manifest in %s\n", target)
	if err := os.MkdirAll(filepath.Join(target, application.Version), 0o755); err != nil {
		return err
	}
	if err := generateFileFromTemplate("application.yaml", application, filepath.Join(target, application.Version, "application.yaml")); err != nil {
		return err
	}

	if err := generateFileFromTemplate("tests.yaml", application, filepath.Join(target, application.Version, "tests.yaml")); err != nil {
		return err
	}
	if application.ReleasePlan {
		if err := generateFileFromTemplate("release-plan.yaml", application, filepath.Join(target, application.Version, "release-plan.yaml")); err != nil {
			return err
		}
	}
	for _, c := range application.Components {
		updateComponent(application, &c)
		if err := generateFileFromTemplate("component.yaml", c, filepath.Join(target, application.Version, fmt.Sprintf("component-%s.yaml", c.Name))); err != nil {
			return err
		}
		if err := generateFileFromTemplate("image.yaml", c, filepath.Join(target, application.Version, fmt.Sprintf("image-%s.yaml", c.Name))); err != nil {
			return err
		}
	}
	return nil
}

func generateGitHub(app k.Application, target string) error {
	log.Printf("Generate github manifests in %s\n", target)
	if err := os.MkdirAll(filepath.Join(target, "workflows"), 0o755); err != nil {
		return err
	}
	filename := fmt.Sprintf("update-sources.%s.yaml", app.Branch)
	if err := generateFileFromTemplate("update-sources.yaml", app, filepath.Join(target, "workflows", filename)); err != nil {
		return err
	}
	return nil
}

func eval(tmpl string, data interface{}) (string, error) {
	var funcMap = template.FuncMap{
		"hyphenize": hyphenize,
		"basename":  basename,
		"indent":    indent,
		"contains":  strings.Contains,
	}
	t, err := template.New("inner").Funcs(funcMap).Parse(tmpl)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	err = t.Execute(&buf, data)
	if err != nil {
		return "", err
	}
	return buf.String(), nil
}

func generateFileFromTemplate(templateFile string, o interface{}, filepath string) error {
	var funcMap = template.FuncMap{
		"hyphenize": hyphenize,
		"basename":  basename,
		"indent":    indent,
		"contains":  strings.Contains,
		"eval":      eval,
	}
	tmpl, err := template.New(templateFile).Funcs(funcMap).ParseFS(templateFS, "templates/*/*.yaml", "templates/*/*/*.yaml")
	if err != nil {
		return err
	}
	f, err := os.Create(filepath)
	if err != nil {
		return err
	}
	defer f.Close()
	err = tmpl.Execute(f, o)
	if err != nil {
		return err
	}
	return nil
}

func cleanupAutogenerated(ctx context.Context, dir string) error {
	if out, err := run(ctx, dir, "bash", "-c", "grep -l -r '# Generated by openshift-pipelines/hack. DO NOT EDIT.' .tekton .konflux .github"); err != nil {
		return fmt.Errorf("Couldn't grep for autogenerated content: %s, %s", err, out)
	} else {
		for _, f := range strings.Split(string(out), "\n") {
			if f == "" {
				continue
			}
			if err := os.Remove(filepath.Join(dir, f)); err != nil {
				return fmt.Errorf("Couldn't remove autogenerated file %s: %w", f, err)
			}
		}
	}
	return nil
}

func hyphenize(str string) string {
	return nameFieldInvalidCharPattern.ReplaceAllString(str, "-")
}

func basename(str string) string {
	return path.Base(str)
}

func indent(spaces int, v string) string {
	pad := strings.Repeat(" ", spaces)
	return pad + strings.Replace(v, "\n", "\n"+pad, -1)
}

type arrayFlags []string

// String is an implementation of the flag.Value interface
func (i *arrayFlags) String() string {
	return fmt.Sprintf("%v", *i)
}

// Set is an implementation of the flag.Value interface
func (i *arrayFlags) Set(value string) error {
	*i = append(*i, value)
	return nil
}

func exists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// Local Variables:
// compile-command: "go run . -target /tmp/foo -config ../../config/konflux/operator.yaml"
// End:
