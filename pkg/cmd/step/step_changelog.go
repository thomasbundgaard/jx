package step

import (
	"bufio"
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/jenkins-x/jx/pkg/cmd/helper"
	"github.com/jenkins-x/jx/pkg/gits/releases"

	"github.com/pkg/errors"

	"github.com/jenkins-x/jx/pkg/users"

	"github.com/ghodss/yaml"
	jenkinsio "github.com/jenkins-x/jx/pkg/apis/jenkins.io"
	v1 "github.com/jenkins-x/jx/pkg/apis/jenkins.io/v1"
	"github.com/jenkins-x/jx/pkg/cmd/opts"
	"github.com/jenkins-x/jx/pkg/cmd/templates"
	"github.com/jenkins-x/jx/pkg/gits"
	"github.com/jenkins-x/jx/pkg/issues"
	"github.com/jenkins-x/jx/pkg/kube"
	"github.com/jenkins-x/jx/pkg/log"
	"github.com/jenkins-x/jx/pkg/util"
	"github.com/spf13/cobra"
	"gopkg.in/src-d/go-git.v4/plumbing/object"

	chgit "github.com/antham/chyle/chyle/git"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// StepChangelogOptions contains the command line flags
type StepChangelogOptions struct {
	opts.StepOptions

	PreviousRevision    string
	PreviousDate        string
	CurrentRevision     string
	TemplatesDir        string
	ReleaseYamlFile     string
	CrdYamlFile         string
	Dir                 string
	Version             string
	Build               string
	Header              string
	HeaderFile          string
	Footer              string
	FooterFile          string
	OutputMarkdownFile  string
	OverwriteCRD        bool
	GenerateCRD         bool
	GenerateReleaseYaml bool
	UpdateRelease       bool
	NoReleaseInDev      bool
	IncludeMergeCommits bool
	FailIfFindCommits   bool
	State               StepChangelogState
}

type StepChangelogState struct {
	GitInfo         *gits.GitRepository
	GitProvider     gits.GitProvider
	Tracker         issues.IssueProvider
	FoundIssueNames map[string]bool
	LoggedIssueKind bool
	Release         *v1.Release
}

const (
	ReleaseName = `{{ .Chart.Name }}-{{ .Chart.Version | replace "+" "_" }}`

	SpecName    = `{{ .Chart.Name }}`
	SpecVersion = `{{ .Chart.Version }}`

	ReleaseCrdYaml = `apiVersion: apiextensions.k8s.io/v1beta1
kind: CustomResourceDefinition
metadata:
  creationTimestamp: 2018-02-24T14:56:33Z
  name: releases.jenkins.io
  resourceVersion: "557150"
  selfLink: /apis/apiextensions.k8s.io/v1beta1/customresourcedefinitions/releases.jenkins.io
  uid: e77f4e08-1972-11e8-988e-42010a8401df
spec:
  group: jenkins.io
  names:
    kind: Release
    listKind: ReleaseList
    plural: releases
    shortNames:
    - rel
    singular: release
    categories:
    - all
  scope: Namespaced
  version: v1`
)

var (
	GitAccessDescription = `

By default jx commands look for a file '~/.jx/gitAuth.yaml' to find the API tokens for Git servers. You can use 'jx create git token' to create a Git token.

Alternatively if you are running this command inside a CI server you can use environment variables to specify the username and API token.
e.g. define environment variables GIT_USERNAME and GIT_API_TOKEN
`

	StepChangelogLong = templates.LongDesc(`
		Generates a Changelog for the latest tag

		This command will generate a Changelog as markdown for the git commit range given. 
		If you are using GitHub it will also update the GitHub Release with the changelog. You can disable that by passing'--update-release=false'

		If you have just created a git tag this command will try default to the changes between the last tag and the previous one. You can always specify the exact Git references (tag/sha) directly via '--previous-rev' and '--rev'

		The changelog is generated by parsing the git commits. It will also detect any text like 'fixes #123' to link to issue fixes. You can also use Conventional Commits notation: https://conventionalcommits.org/ to get a nicer formatted changelog. e.g. using commits like 'fix:(my feature) this my fix' or 'feat:(cheese) something'

		This command also generates a Release Custom Resource Definition you can include in your helm chart to give metadata about the changelog of the application along with metadata about the release (git tag, url, commits, issues fixed etc). Including this metadata in a helm charts means we can do things like automatically comment on issues when they hit Staging or Production; or give detailed descriptions of what things have changed when using GitOps to update versions in an environment by referencing the fixed issues in the Pull Request.

		You can opt out of the release YAML generation via the '--generate-yaml=false' option
		
		To update the release notes on GitHub / Gitea this command needs a git API token.

`) + GitAccessDescription

	StepChangelogExample = templates.Examples(`
		# generate a changelog on the current source
		jx step changelog

		# specify the version to use
		jx step changelog --version 1.2.3

		# specify the version and a header template
		jx step changelog --header-file docs/dev/changelog-header.md --version 1.2.3

`)

	GitHubIssueRegex = regexp.MustCompile(`(\#\d+)`)
	JIRAIssueRegex   = regexp.MustCompile(`[A-Z][A-Z]+-(\d+)`)
)

func NewCmdStepChangelog(commonOpts *opts.CommonOptions) *cobra.Command {
	options := StepChangelogOptions{
		StepOptions: opts.StepOptions{
			CommonOptions: commonOpts,
		},
	}
	cmd := &cobra.Command{
		Use:     "changelog",
		Short:   "Creates a changelog for a git tag",
		Aliases: []string{"changes"},
		Long:    StepChangelogLong,
		Example: StepChangelogExample,
		Run: func(cmd *cobra.Command, args []string) {
			options.Cmd = cmd
			options.Args = args
			err := options.Run()
			helper.CheckErr(err)
		},
	}

	cmd.Flags().StringVarP(&options.PreviousRevision, "previous-rev", "p", "", "the previous tag revision")
	cmd.Flags().StringVarP(&options.PreviousDate, "previous-date", "", "", "the previous date to find a revision in format 'MonthName dayNumber year'")
	cmd.Flags().StringVarP(&options.CurrentRevision, "rev", "r", "", "the current tag revision")
	cmd.Flags().StringVarP(&options.TemplatesDir, "templates-dir", "t", "", "the directory containing the helm chart templates to generate the resources")
	cmd.Flags().StringVarP(&options.ReleaseYamlFile, "release-yaml-file", "", "release.yaml", "the name of the file to generate the Release YAML")
	cmd.Flags().StringVarP(&options.CrdYamlFile, "crd-yaml-file", "", "release-crd.yaml", "the name of the file to generate the Release CustomResourceDefinition YAML")
	cmd.Flags().StringVarP(&options.Version, "version", "v", "", "The version to release")
	cmd.Flags().StringVarP(&options.Build, "build", "", "", "The Build number which is used to update the PipelineActivity. If not specified its defaulted from  the '$BUILD_NUMBER' environment variable")
	cmd.Flags().StringVarP(&options.Dir, "dir", "", "", "The directory of the Git repository. Defaults to the current working directory")
	cmd.Flags().StringVarP(&options.OutputMarkdownFile, "output-markdown", "", "", "The file to generate for the changelog output if not updating a Git provider release")
	cmd.Flags().BoolVarP(&options.OverwriteCRD, "overwrite", "o", false, "overwrites the Release CRD YAML file if it exists")
	cmd.Flags().BoolVarP(&options.GenerateCRD, "crd", "c", false, "Generate the CRD in the chart")
	cmd.Flags().BoolVarP(&options.GenerateReleaseYaml, "generate-yaml", "y", true, "Generate the Release YAML in the local helm chart")
	cmd.Flags().BoolVarP(&options.UpdateRelease, "update-release", "", true, "Should we update the release on the Git repository with the changelog")
	cmd.Flags().BoolVarP(&options.NoReleaseInDev, "no-dev-release", "", false, "Disables the generation of Release CRDs in the development namespace to track releases being performed")
	cmd.Flags().BoolVarP(&options.IncludeMergeCommits, "include-merge-commits", "", false, "Include merge commits when generating the changelog")
	cmd.Flags().BoolVarP(&options.FailIfFindCommits, "fail-if-no-commits", "", false, "Do we want to fail the build if we don't find any commits to generate the changelog")

	cmd.Flags().StringVarP(&options.Header, "header", "", "", "The changelog header in markdown for the changelog. Can use go template expressions on the ReleaseSpec object: https://golang.org/pkg/text/template/")
	cmd.Flags().StringVarP(&options.HeaderFile, "header-file", "", "", "The file name of the changelog header in markdown for the changelog. Can use go template expressions on the ReleaseSpec object: https://golang.org/pkg/text/template/")
	cmd.Flags().StringVarP(&options.Footer, "footer", "", "", "The changelog footer in markdown for the changelog. Can use go template expressions on the ReleaseSpec object: https://golang.org/pkg/text/template/")
	cmd.Flags().StringVarP(&options.FooterFile, "footer-file", "", "", "The file name of the changelog footer in markdown for the changelog. Can use go template expressions on the ReleaseSpec object: https://golang.org/pkg/text/template/")

	return cmd
}

func (o *StepChangelogOptions) Run() error {
	// lets enable batch mode if we detect we are inside a pipeline
	if !o.BatchMode && o.GetBuildNumber() != "" {
		log.Logger().Info("Using batch mode as inside a pipeline")
		o.BatchMode = true
	}

	apisClient, err := o.ApiExtensionsClient()
	if err != nil {
		return err
	}
	err = kube.RegisterPipelineActivityCRD(apisClient)
	if err != nil {
		return err
	}
	err = kube.RegisterGitServiceCRD(apisClient)
	if err != nil {
		return err
	}
	err = kube.RegisterReleaseCRD(apisClient)
	if err != nil {
		return err
	}
	err = o.RegisterUserCRD()
	if err != nil {
		return err
	}

	dir := o.Dir
	if dir == "" {
		dir, err = os.Getwd()
		if err != nil {
			return err
		}
	}

	// Ensure we don't have a shallow checkout in git
	err = gits.Unshallow(dir, o.Git())
	if err != nil {
		return errors.Wrapf(err, "error unshallowing git repo in %s", dir)
	}
	previousRev := o.PreviousRevision
	if previousRev == "" {
		previousDate := o.PreviousDate
		if previousDate != "" {
			previousRev, err = o.Git().GetRevisionBeforeDateText(dir, previousDate)
			if err != nil {
				return fmt.Errorf("Failed to find commits before date %s: %s", previousDate, err)
			}
		}
	}
	if previousRev == "" {
		previousRev, err = o.Git().GetPreviousGitTagSHA(dir)
		if err != nil {
			return err
		}
		if previousRev == "" {
			log.Logger().Info("no previous commit version found so change diff unavailable")
			return nil
		}
	}
	currentRev := o.CurrentRevision
	if currentRev == "" {
		currentRev, err = o.Git().GetCurrentGitTagSHA(dir)
		if err != nil {
			return err
		}
	}

	templatesDir := o.TemplatesDir
	if templatesDir == "" {
		chartFile, err := o.FindHelmChart()
		if err != nil {
			return fmt.Errorf("Could not find helm chart %s", err)
		}
		path, _ := filepath.Split(chartFile)
		templatesDir = filepath.Join(path, "templates")
	}
	err = os.MkdirAll(templatesDir, util.DefaultWritePermissions)
	if err != nil {
		return fmt.Errorf("Failed to create the templates directory %s due to %s", templatesDir, err)
	}

	log.Logger().Infof("Generating change log from git ref %s => %s", util.ColorInfo(previousRev), util.ColorInfo(currentRev))

	gitDir, gitConfDir, err := o.Git().FindGitConfigDir(dir)
	if err != nil {
		return err
	}
	if gitDir == "" || gitConfDir == "" {
		log.Logger().Warnf("No git directory could be found from dir %s", dir)
		return nil
	}

	gitUrl, err := o.Git().DiscoverUpstreamGitURL(gitConfDir)
	if err != nil {
		return err
	}
	gitInfo, err := gits.ParseGitURL(gitUrl)
	if err != nil {
		return err
	}
	o.State.GitInfo = gitInfo

	tracker, err := o.CreateIssueProvider(dir)
	if err != nil {
		return err
	}
	o.State.Tracker = tracker

	authConfigSvc, err := o.CreateGitAuthConfigService()
	if err != nil {
		return err
	}
	jxClient, devNs, err := o.JXClientAndDevNamespace()
	if err != nil {
		return err
	}

	gitKind, err := o.GitServerKind(gitInfo)
	foundGitProvider := true
	gitProvider, err := o.State.GitInfo.CreateProvider(o.InCluster(), authConfigSvc, gitKind, o.Git(), o.BatchMode, o.In, o.Out, o.Err)
	if err != nil {
		foundGitProvider = false
		log.Logger().Warnf("Could not create GitProvide so cannot update the release notes: %s", err)
	}
	o.State.GitProvider = gitProvider
	o.State.FoundIssueNames = map[string]bool{}

	commits, err := chgit.FetchCommits(gitDir, previousRev, currentRev)
	if err != nil {
		if o.FailIfFindCommits {
			return err
		}
		log.Logger().Warnf("failed to find git commits between revision %s and %s due to: %s", previousRev, currentRev, err.Error())
	}
	if commits != nil {
		commits1 := *commits
		if len(commits1) > 0 {
			if strings.HasPrefix(commits1[0].Message, "release ") {
				// remove the release commit from the log
				tmp := commits1[1:]
				commits = &tmp
			}
		}
		log.Logger().Debugf("Found commits:")
		for _, commit := range *commits {
			log.Logger().Debugf("  commit %s", commit.Hash)
			log.Logger().Debugf("  Author: %s <%s>", commit.Author.Name, commit.Author.Email)
			log.Logger().Debugf("  Date: %s", commit.Committer.When.Format(time.ANSIC))
			log.Logger().Debugf("      %s\n\n\n", commit.Message)
		}
	}
	version := o.Version
	if version == "" {
		version = SpecVersion
	}

	release := &v1.Release{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Release",
			APIVersion: jenkinsio.GroupAndVersion,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: ReleaseName,
			CreationTimestamp: metav1.Time{
				Time: time.Now(),
			},
			//ResourceVersion:   "1",
			DeletionTimestamp: &metav1.Time{},
		},
		Spec: v1.ReleaseSpec{
			Name:          SpecName,
			Version:       version,
			GitOwner:      gitInfo.Organisation,
			GitRepository: gitInfo.Name,
			GitHTTPURL:    gitInfo.HttpsURL(),
			GitCloneURL:   gitInfo.HttpCloneURL(),
			Commits:       []v1.CommitSummary{},
			Issues:        []v1.IssueSummary{},
			PullRequests:  []v1.IssueSummary{},
		},
	}

	resolver := users.GitUserResolver{
		GitProvider: gitProvider,
		Namespace:   devNs,
		JXClient:    jxClient,
	}
	if commits != nil {
		for _, commit := range *commits {

			if o.IncludeMergeCommits || len(commit.ParentHashes) <= 1 {
				o.addCommit(&release.Spec, &commit, &resolver)
			}
		}
	}

	// lets try to update the release
	markdown, err := gits.GenerateMarkdown(&release.Spec, gitInfo)
	if err != nil {
		return err
	}
	header, err := o.getTemplateResult(&release.Spec, "header", o.Header, o.HeaderFile)
	if err != nil {
		return err
	}
	footer, err := o.getTemplateResult(&release.Spec, "footer", o.Footer, o.FooterFile)
	if err != nil {
		return err
	}
	markdown = header + markdown + footer

	log.Logger().Debugf("Generated release notes:\n\n%s\n", markdown)

	if version != "" && o.UpdateRelease && foundGitProvider {
		releaseInfo := &gits.GitRelease{
			Name:    version,
			TagName: version,
			Body:    markdown,
		}
		url := releaseInfo.HTMLURL
		if url == "" {
			url = releaseInfo.URL
		}
		if url == "" {
			url = util.UrlJoin(gitInfo.HttpsURL(), "releases/tag", version)
		}
		err = gitProvider.UpdateRelease(gitInfo.Organisation, gitInfo.Name, version, releaseInfo)
		if err != nil {
			log.Logger().Warnf("Failed to update the release at %s: %s", url, err)
			return nil
		}
		release.Spec.ReleaseNotesURL = url
		log.Logger().Infof("Updated the release information at %s", util.ColorInfo(url))
	} else if o.OutputMarkdownFile != "" {
		err := ioutil.WriteFile(o.OutputMarkdownFile, []byte(markdown), util.DefaultWritePermissions)
		if err != nil {
			return err
		}
		log.Logger().Infof("\nGenerated Changelog: %s", util.ColorInfo(o.OutputMarkdownFile))
	} else {
		log.Logger().Infof("\nGenerated Changelog:")
		log.Logger().Infof("%s\n", markdown)
	}

	o.State.Release = release
	// now lets marshal the release YAML
	data, err := yaml.Marshal(release)
	if err != nil {
		return err
	}
	if data == nil {
		return fmt.Errorf("Could not marshal release to yaml")
	}
	releaseFile := filepath.Join(templatesDir, o.ReleaseYamlFile)
	crdFile := filepath.Join(templatesDir, o.CrdYamlFile)
	if o.GenerateReleaseYaml {
		err = ioutil.WriteFile(releaseFile, data, util.DefaultWritePermissions)
		if err != nil {
			return fmt.Errorf("Failed to save Release YAML file %s: %s", releaseFile, err)
		}
		log.Logger().Infof("generated: %s", util.ColorInfo(releaseFile))
	}
	cleanVersion := strings.TrimPrefix(version, "v")
	release.Spec.Version = cleanVersion
	if o.GenerateCRD {
		exists, err := util.FileExists(crdFile)
		if err != nil {
			return fmt.Errorf("Failed to check for CRD YAML file %s: %s", crdFile, err)
		}
		if o.OverwriteCRD || !exists {
			err = ioutil.WriteFile(crdFile, []byte(ReleaseCrdYaml), util.DefaultWritePermissions)
			if err != nil {
				return fmt.Errorf("Failed to save Release CRD YAML file %s: %s", crdFile, err)
			}
			log.Logger().Infof("generated: %s", util.ColorInfo(crdFile))
		}
	}
	appName := ""
	if gitInfo != nil {
		appName = gitInfo.Name
	}
	if appName == "" {
		appName = release.Spec.Name
	}
	if appName == "" {
		appName = release.Spec.GitRepository
	}
	if !o.NoReleaseInDev {
		devRelease := *release
		devRelease.ResourceVersion = ""
		devRelease.Namespace = devNs
		devRelease.Name = kube.ToValidName(appName + "-" + cleanVersion)
		devRelease.Spec.Name = appName
		_, err := kube.GetOrCreateRelease(jxClient, devNs, &devRelease)
		if err != nil {
			log.Logger().Warnf("%s", err)
		} else {
			log.Logger().Infof("Created Release %s resource in namespace %s", devRelease.Name, devNs)
		}
	}
	releaseNotesURL := release.Spec.ReleaseNotesURL
	pipeline := ""
	build := o.Build
	pipeline, build = o.GetPipelineName(gitInfo, pipeline, build, appName)
	if pipeline != "" && build != "" {
		name := kube.ToValidName(pipeline + "-" + build)
		// lets see if we can update the pipeline
		activities := jxClient.JenkinsV1().PipelineActivities(devNs)
		lastCommitSha := ""
		lastCommitMessage := ""
		lastCommitURL := ""
		commits := release.Spec.Commits
		if len(commits) > 0 {
			lastCommit := commits[len(commits)-1]
			lastCommitSha = lastCommit.SHA
			lastCommitMessage = lastCommit.Message
			lastCommitURL = lastCommit.URL
		}
		log.Logger().Infof("Updating PipelineActivity %s with version %s", name, cleanVersion)

		key := &kube.PromoteStepActivityKey{
			PipelineActivityKey: kube.PipelineActivityKey{
				Name:              name,
				Pipeline:          pipeline,
				Build:             build,
				ReleaseNotesURL:   releaseNotesURL,
				LastCommitSHA:     lastCommitSha,
				LastCommitMessage: lastCommitMessage,
				LastCommitURL:     lastCommitURL,
				Version:           cleanVersion,
				GitInfo:           gitInfo,
			},
		}
		_, currentNamespace, err := o.KubeClientAndNamespace()
		if err != nil {
			return errors.Wrap(err, "getting current namespace")
		}
		a, created, err := key.GetOrCreate(jxClient, currentNamespace)
		if err == nil && a != nil && !created {
			_, err = activities.PatchUpdate(a)
			if err != nil {
				log.Logger().Warnf("Failed to update PipelineActivities %s: %s", name, err)
			} else {
				log.Logger().Infof("Updated PipelineActivities %s with release notes URL: %s", util.ColorInfo(name), util.ColorInfo(releaseNotesURL))
			}
		}
	} else {
		log.Logger().Infof("No pipeline and build number available on $JOB_NAME and $BUILD_NUMBER so cannot update PipelineActivities with the ReleaseNotesURL")
	}
	return nil
}

func (o *StepChangelogOptions) addCommit(spec *v1.ReleaseSpec, commit *object.Commit, resolver *users.GitUserResolver) {
	// TODO
	url := ""
	branch := "master"

	var author, committer *v1.User
	var err error
	sha := commit.Hash.String()
	if commit.Author.Email != "" && commit.Author.Name != "" {
		author, err = resolver.GitSignatureAsUser(&commit.Author)
		if err != nil {
			log.Logger().Warnf("Failed to enrich commit %s with issues: %v", sha, err)
		}
	}
	if commit.Committer.Email != "" && commit.Committer.Name != "" {
		committer, err = resolver.GitSignatureAsUser(&commit.Committer)
		if err != nil {
			log.Logger().Warnf("Failed to enrich commit %s with issues: %v", sha, err)
		}
	}
	var authorDetails, committerDetails v1.UserDetails
	if author != nil {
		authorDetails = author.Spec
	}
	if committer != nil {
		committerDetails = committer.Spec
	}
	commitSummary := v1.CommitSummary{
		Message:   commit.Message,
		URL:       url,
		SHA:       sha,
		Author:    &authorDetails,
		Branch:    branch,
		Committer: &committerDetails,
	}
	err = o.addIssuesAndPullRequests(spec, &commitSummary, commit)
	if err != nil {
		log.Logger().Warnf("Failed to enrich commit %s with issues: %s", sha, err)
	}
	spec.Commits = append(spec.Commits, commitSummary)

}

func (o *StepChangelogOptions) addIssuesAndPullRequests(spec *v1.ReleaseSpec, commit *v1.CommitSummary, rawCommit *object.Commit) error {
	tracker := o.State.Tracker

	gitProvider := o.State.GitProvider
	if gitProvider == nil || !gitProvider.HasIssues() {
		return nil
	}
	regex := GitHubIssueRegex
	issueKind := issues.GetIssueProvider(tracker)
	if !o.State.LoggedIssueKind {
		o.State.LoggedIssueKind = true
		log.Logger().Infof("Finding issues in commit messages using %s format", issueKind)
	}
	if issueKind == issues.Jira {
		regex = JIRAIssueRegex
	}
	message := fullCommitMessageText(rawCommit)

	dependencyUpdate, err := releases.ParseDependencyUpdateMessage(message)
	if err != nil {
		log.Logger().Infof("Parsing %s for dependency updates", message)
	}
	if dependencyUpdate != nil {
		message, err = o.inlineDependencyUpdateMessage(message, dependencyUpdate)
		if err != nil {
			log.Logger().Warnf("Unable to inline dependency update message for %s %v", dependencyUpdate.String(), err)
		}
	}

	matches := regex.FindAllStringSubmatch(message, -1)
	jxClient, ns, err := o.JXClientAndDevNamespace()
	if err != nil {
		return err
	}
	resolver := users.GitUserResolver{
		JXClient:    jxClient,
		Namespace:   ns,
		GitProvider: gitProvider,
	}
	for _, match := range matches {
		for _, result := range match {
			result = strings.TrimPrefix(result, "#")
			if _, ok := o.State.FoundIssueNames[result]; !ok {
				o.State.FoundIssueNames[result] = true
				issue, err := tracker.GetIssue(result)
				if err != nil {
					log.Logger().Warnf("Failed to lookup issue %s in issue tracker %s due to %s", result, tracker.HomeURL(), err)
					continue
				}
				if issue == nil {
					log.Logger().Warnf("Failed to find issue %s for repository %s", result, tracker.HomeURL())
					continue
				}

				var user v1.UserDetails
				if issue.User == nil {
					log.Logger().Warnf("Failed to find user for issue %s repository %s", result, tracker.HomeURL())
				} else {
					u, err := resolver.Resolve(issue.User)
					if err != nil {
						return err
					}
					if u != nil {
						user = u.Spec
					}
				}

				var closedBy v1.UserDetails
				if issue.ClosedBy == nil {
					log.Logger().Warnf("Failed to find closedBy user for issue %s repository %s", result, tracker.HomeURL())
				} else {
					u, err := resolver.Resolve(issue.User)
					if err != nil {
						return err
					}
					if u != nil {
						closedBy = u.Spec
					}
				}

				var assignees []v1.UserDetails
				if issue.Assignees == nil {
					log.Logger().Warnf("Failed to find assignees for issue %s repository %s", result, tracker.HomeURL())
				} else {
					u, err := resolver.GitUserSliceAsUserDetailsSlice(issue.Assignees)
					if err != nil {
						return err
					}
					assignees = u
				}

				labels := toV1Labels(issue.Labels)
				commit.IssueIDs = append(commit.IssueIDs, result)
				issueSummary := v1.IssueSummary{
					ID:                result,
					URL:               issue.URL,
					Title:             issue.Title,
					Body:              issue.Body,
					User:              &user,
					CreationTimestamp: kube.ToMetaTime(issue.CreatedAt),
					ClosedBy:          &closedBy,
					Assignees:         assignees,
					Labels:            labels,
				}
				state := issue.State
				if state != nil {
					issueSummary.State = *state
				}
				if issue.IsPullRequest {
					spec.PullRequests = append(spec.PullRequests, issueSummary)
				} else {
					spec.Issues = append(spec.Issues, issueSummary)
				}
			}
		}
	}
	return nil
}

func (o *StepChangelogOptions) inlineDependencyUpdateMessage(msg string, update *releases.DependencyUpdate) (string, error) {
	if update.Owner == "" {
		update.Owner = o.State.GitInfo.Organisation
	}
	if update.Host == "" {
		update.Host = o.State.GitInfo.Host
	}
	if update.URL == "" {
		update.URL = fmt.Sprintf("https://%s/%s/%s", update.Host, update.Owner, update.Repo)
	}
	provider, _, err := o.CreateGitProviderForURLWithoutKind(update.URL)
	if err != nil {
		return "", errors.Wrapf(err, "creating git provider for %s", update.URL)
	}
	release, err := provider.GetRelease(update.Owner, update.Repo, update.ToVersion)
	if err != nil {
		// normally tags are v<version> so try that
		tag := fmt.Sprintf("v%s", update.ToVersion)
		release, err = provider.GetRelease(update.Owner, update.Repo, tag)
		if err != nil {
			return "", errors.Wrapf(err, "getting release for %s (tried %s and v%s)", update.ToVersion, update.ToVersion, update.ToVersion)
		}
	}
	return fmt.Sprintf(`%s

<details open>
%s
</details>
`, msg, release.Body), nil
}

// toV1Labels converts git labels to IssueLabel
func toV1Labels(labels []gits.GitLabel) []v1.IssueLabel {
	answer := []v1.IssueLabel{}
	for _, label := range labels {
		answer = append(answer, v1.IssueLabel{
			URL:   label.URL,
			Name:  label.Name,
			Color: label.Color,
		})
	}
	return answer
}

// fullCommitMessageText returns the commit message
func fullCommitMessageText(commit *object.Commit) string {
	answer := commit.Message
	fn := func(parent *object.Commit) error {
		text := parent.Message
		if text != "" {
			sep := "\n"
			if strings.HasSuffix(answer, "\n") {
				sep = ""
			}
			answer += sep + text
		}
		return nil
	}
	fn(commit)
	return answer

}

func (o *StepChangelogOptions) getTemplateResult(releaseSpec *v1.ReleaseSpec, templateName string, templateText string, templateFile string) (string, error) {
	if templateText == "" {
		if templateFile == "" {
			return "", nil
		}
		data, err := ioutil.ReadFile(templateFile)
		if err != nil {
			return "", err
		}
		templateText = string(data)
	}
	if templateText == "" {
		return "", nil
	}
	tmpl, err := template.New(templateName).Parse(templateText)
	if err != nil {
		return "", err
	}
	var buffer bytes.Buffer
	writer := bufio.NewWriter(&buffer)
	err = tmpl.Execute(writer, releaseSpec)
	writer.Flush()
	return buffer.String(), err
}
