// Copyright 2026 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package githubmetadata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"forgejo.org/models"
	"forgejo.org/models/db"
	issues_model "forgejo.org/models/issues"
	repo_model "forgejo.org/models/repo"
	"forgejo.org/models/unit"
	user_model "forgejo.org/models/user"
	"forgejo.org/modules/git"
	"forgejo.org/modules/label"
	"forgejo.org/modules/log"
	"forgejo.org/modules/setting"
	"forgejo.org/modules/timeutil"
	"forgejo.org/modules/util"
)

const (
	defaultInterval = 12 * time.Hour

	sourceKindIssue         = "issue"
	sourceKindPull          = "pull"
	sourceKindIssueComment  = "issue_comment"
	sourceKindReview        = "review"
	sourceKindReviewComment = "review_comment"
)

type githubUser struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
}

type githubLabel struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

type githubIssue struct {
	ID        int64         `json:"id"`
	Number    int64         `json:"number"`
	State     string        `json:"state"`
	Title     string        `json:"title"`
	Body      string        `json:"body"`
	User      *githubUser   `json:"user"`
	Labels    []githubLabel `json:"labels"`
	Locked    bool          `json:"locked"`
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at"`
	ClosedAt  *time.Time    `json:"closed_at"`
}

type githubPull struct {
	ID             int64         `json:"id"`
	Number         int64         `json:"number"`
	State          string        `json:"state"`
	Title          string        `json:"title"`
	Body           string        `json:"body"`
	User           *githubUser   `json:"user"`
	Labels         []githubLabel `json:"labels"`
	Locked         bool          `json:"locked"`
	Draft          bool          `json:"draft"`
	Merged         bool          `json:"merged"`
	MergeCommitSHA string        `json:"merge_commit_sha"`
	Head           githubRef     `json:"head"`
	Base           githubRef     `json:"base"`
	CreatedAt      time.Time     `json:"created_at"`
	UpdatedAt      time.Time     `json:"updated_at"`
	ClosedAt       *time.Time    `json:"closed_at"`
	MergedAt       *time.Time    `json:"merged_at"`
}

type githubRef struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

type githubEvent struct {
	Event       string      `json:"event"`
	ID          int64       `json:"id"`
	User        *githubUser `json:"user"`
	Actor       *githubUser `json:"actor"`
	Body        string      `json:"body"`
	State       string      `json:"state"`
	CommitID    string      `json:"commit_id"`
	CreatedAt   *time.Time  `json:"created_at"`
	UpdatedAt   *time.Time  `json:"updated_at"`
	SubmittedAt *time.Time  `json:"submitted_at"`
}

type githubReviewComment struct {
	ID                  int64       `json:"id"`
	PullRequestReviewID int64       `json:"pull_request_review_id"`
	User                *githubUser `json:"user"`
	Body                string      `json:"body"`
	DiffHunk            string      `json:"diff_hunk"`
	Path                string      `json:"path"`
	CommitID            string      `json:"commit_id"`
	OriginalCommitID    string      `json:"original_commit_id"`
	Line                *int        `json:"line"`
	OriginalLine        *int        `json:"original_line"`
	StartLine           *int        `json:"start_line"`
	OriginalStartLine   *int        `json:"original_start_line"`
	Side                string      `json:"side"`
	CreatedAt           time.Time   `json:"created_at"`
	UpdatedAt           time.Time   `json:"updated_at"`
}

type issueFile struct {
	Issue  githubIssue   `json:"issue"`
	Events []githubEvent `json:"events"`
}

type pullFile struct {
	Pull     githubPull            `json:"pull"`
	Events   []githubEvent         `json:"events"`
	Comments []githubReviewComment `json:"comments"`
}

type backupState struct {
	LastBackup string `json:"last_backup"`
}

type importer struct {
	repo   *repo_model.Repository
	labels map[string]*issues_model.Label
}

// DefaultInterval returns the default metadata mirror interval.
func DefaultInterval() time.Duration {
	return defaultInterval
}

// ValidateMetadataPath verifies that path points at a local GitHub metadata backup clone.
func ValidateMetadataPath(metadataPath string) error {
	metadataPath = strings.TrimSpace(metadataPath)
	if metadataPath == "" {
		return fmt.Errorf("metadata path is required")
	}
	if !filepath.IsAbs(metadataPath) {
		return fmt.Errorf("metadata path must be absolute")
	}
	for _, rel := range []string{"state.json", "issues", "pulls", ".git"} {
		if _, err := os.Stat(filepath.Join(metadataPath, rel)); err != nil {
			return fmt.Errorf("metadata path is not a GitHub metadata backup clone: %w", err)
		}
	}
	return nil
}

// MetadataSourceLocalPath returns the absolute path when source is a local path.
func MetadataSourceLocalPath(source string) (string, bool) {
	source = strings.TrimSpace(source)
	if filepath.IsAbs(source) {
		return source, true
	}
	u, err := url.Parse(source)
	if err == nil && u.Scheme == "file" {
		return u.Host + u.Path, true
	}
	return "", false
}

// ValidateMetadataSource verifies that source is a backup clone path or URL.
func ValidateMetadataSource(source string) error {
	source = strings.TrimSpace(source)
	if source == "" {
		return fmt.Errorf("metadata source is required")
	}
	if metadataPath, ok := MetadataSourceLocalPath(source); ok {
		return ValidateMetadataPath(metadataPath)
	}
	u, err := url.Parse(source)
	if err != nil || u.Scheme == "" {
		return fmt.Errorf("metadata source must be an absolute server path or clone URL")
	}
	return nil
}

func managedMetadataPath(repoID int64) string {
	return filepath.Join(setting.AppDataPath, "github-metadata-mirrors", strconv.FormatInt(repoID, 10))
}

// CreateMirror registers a metadata mirror for a repository.
func CreateMirror(ctx context.Context, repoID int64, metadataSource string, interval time.Duration) error {
	metadataSource = strings.TrimSpace(metadataSource)
	if err := ValidateMetadataSource(metadataSource); err != nil {
		return err
	}
	metadataPath := managedMetadataPath(repoID)
	if localPath, ok := MetadataSourceLocalPath(metadataSource); ok {
		metadataSource = localPath
		metadataPath = localPath
	}

	repo, err := repo_model.GetRepositoryByID(ctx, repoID)
	if err != nil {
		return err
	}
	if err := EnsureMetadataUnits(ctx, repo); err != nil {
		return err
	}

	return repo_model.InsertGitHubMetadataMirror(ctx, &repo_model.GitHubMetadataMirror{
		RepoID:       repoID,
		MetadataPath: metadataPath,
		MetadataURL:  metadataSource,
		Interval:     interval,
	})
}

// EnsureMetadataUnits enables the units that display imported GitHub metadata.
func EnsureMetadataUnits(ctx context.Context, repo *repo_model.Repository) error {
	units := []repo_model.RepoUnit{
		{
			RepoID: repo.ID,
			Type:   unit.TypeIssues,
			Config: &repo_model.IssuesConfig{
				EnableTimetracker:                setting.Service.DefaultEnableTimetracking,
				AllowOnlyContributorsToTrackTime: setting.Service.DefaultAllowOnlyContributorsToTrackTime,
				EnableDependencies:               setting.Service.DefaultEnableDependencies,
			},
		},
		{
			RepoID: repo.ID,
			Type:   unit.TypePullRequests,
			Config: &repo_model.PullRequestsConfig{
				DefaultMergeStyle:  repo_model.MergeStyle(setting.Repository.PullRequest.DefaultMergeStyle),
				DefaultUpdateStyle: repo_model.UpdateStyle(setting.Repository.PullRequest.DefaultUpdateStyle),
			},
		},
	}

	for _, repoUnit := range units {
		has, err := db.GetEngine(ctx).Exist(&repo_model.RepoUnit{RepoID: repo.ID, Type: repoUnit.Type})
		if err != nil {
			return err
		}
		if has {
			continue
		}
		if err := db.Insert(ctx, &repoUnit); err != nil {
			return err
		}
	}
	return nil
}

// SyncAfterMigration imports metadata immediately after the git mirror migration.
func SyncAfterMigration(ctx context.Context, repo *repo_model.Repository) error {
	metadataMirror, err := repo_model.GetGitHubMetadataMirrorByRepoID(ctx, repo.ID)
	if err != nil {
		if err == repo_model.ErrMirrorNotExist {
			return nil
		}
		return err
	}
	return syncMirror(ctx, repo, metadataMirror, true)
}

// Update synchronizes due metadata mirrors.
func Update(ctx context.Context, limit int) error {
	return repo_model.GitHubMetadataMirrorsIterate(ctx, limit, func(_ int, bean any) error {
		metadataMirror := bean.(*repo_model.GitHubMetadataMirror)
		repo, err := repo_model.GetRepositoryByID(ctx, metadataMirror.RepoID)
		if err != nil {
			return err
		}
		if repo.Status != repo_model.RepositoryReady {
			return nil
		}
		return syncMirror(ctx, repo, metadataMirror, false)
	})
}

func syncMirror(ctx context.Context, repo *repo_model.Repository, metadataMirror *repo_model.GitHubMetadataMirror, initial bool) error {
	cloned, err := ensureMetadataCheckout(ctx, metadataMirror)
	if err != nil {
		return err
	}
	if err := EnsureMetadataUnits(ctx, repo); err != nil {
		return err
	}
	if !initial && !cloned {
		if stdout, _, err := git.NewCommand(ctx, "pull", "--ff-only").
			RunStdString(&git.RunOpts{Dir: metadataMirror.MetadataPath}); err != nil {
			return fmt.Errorf("git pull metadata backup: %w: %s", err, stdout)
		}
	}

	currentCommit, err := metadataHead(ctx, metadataMirror.MetadataPath)
	if err != nil {
		return err
	}

	issueFiles, pullFiles, err := metadataFilesChanged(ctx, metadataMirror, currentCommit, initial)
	if err != nil {
		return err
	}

	imp, err := newImporter(ctx, repo)
	if err != nil {
		return err
	}

	for _, filename := range issueFiles {
		if err := imp.importIssueFile(ctx, filename); err != nil {
			return fmt.Errorf("import issue metadata %s: %w", filename, err)
		}
	}
	for _, filename := range pullFiles {
		if err := imp.importPullFile(ctx, filename); err != nil {
			return fmt.Errorf("import pull metadata %s: %w", filename, err)
		}
	}

	lastBackup, err := readLastBackupUnix(metadataMirror.MetadataPath)
	if err != nil {
		return err
	}
	metadataMirror.LastCommit = currentCommit
	metadataMirror.LastBackupUnix = lastBackup
	metadataMirror.UpdatedUnix = timeutil.TimeStampNow()
	metadataMirror.ScheduleNextUpdate()
	if err := repo_model.UpdateGitHubMetadataMirror(ctx, metadataMirror); err != nil {
		return err
	}

	if err := issues_model.RecalculateIssueIndexForRepo(ctx, repo.ID); err != nil {
		return err
	}
	return models.UpdateRepoStats(ctx, repo.ID)
}

func ensureMetadataCheckout(ctx context.Context, metadataMirror *repo_model.GitHubMetadataMirror) (bool, error) {
	source := strings.TrimSpace(metadataMirror.MetadataURL)
	if source == "" {
		source = strings.TrimSpace(metadataMirror.MetadataPath)
	}
	if metadataPath, ok := MetadataSourceLocalPath(source); ok {
		metadataMirror.MetadataPath = metadataPath
		return false, ValidateMetadataPath(metadataPath)
	}

	if metadataMirror.MetadataPath == "" {
		metadataMirror.MetadataPath = managedMetadataPath(metadataMirror.RepoID)
	}
	if err := ValidateMetadataPath(metadataMirror.MetadataPath); err == nil {
		return false, nil
	}
	if filepath.Clean(metadataMirror.MetadataPath) != filepath.Clean(managedMetadataPath(metadataMirror.RepoID)) {
		return false, ValidateMetadataPath(metadataMirror.MetadataPath)
	}
	if err := util.RemoveAll(metadataMirror.MetadataPath); err != nil {
		return false, err
	}
	timeout := time.Duration(setting.Git.Timeout.Migrate) * time.Second
	if err := git.Clone(ctx, source, metadataMirror.MetadataPath, git.CloneRepoOptions{
		Quiet:         true,
		Timeout:       timeout,
		SkipTLSVerify: setting.Migrations.SkipTLSVerify,
	}); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return false, fmt.Errorf("metadata backup clone timed out. Consider increasing [git.timeout] MIGRATE in app.ini. Underlying Error: %w", err)
		}
		return false, fmt.Errorf("clone metadata backup: %w", err)
	}
	return true, ValidateMetadataPath(metadataMirror.MetadataPath)
}

func metadataHead(ctx context.Context, metadataPath string) (string, error) {
	stdout, _, err := git.NewCommand(ctx, "rev-parse", "HEAD").
		RunStdString(&git.RunOpts{Dir: metadataPath})
	if err != nil {
		return "", fmt.Errorf("read metadata backup HEAD: %w", err)
	}
	return strings.TrimSpace(stdout), nil
}

func metadataFilesChanged(ctx context.Context, metadataMirror *repo_model.GitHubMetadataMirror, currentCommit string, initial bool) ([]string, []string, error) {
	if initial || metadataMirror.LastCommit == "" {
		return allMetadataFiles(metadataMirror.MetadataPath)
	}
	if metadataMirror.LastCommit == currentCommit {
		return nil, nil, nil
	}

	rangeSpec := metadataMirror.LastCommit + ".." + currentCommit
	stdout, _, err := git.NewCommand(ctx, "diff", "--name-only").
		AddDynamicArguments(rangeSpec).
		AddDashesAndList("issues", "pulls", "state.json").
		RunStdString(&git.RunOpts{Dir: metadataMirror.MetadataPath})
	if err != nil {
		log.Warn("Unable to diff metadata backup from %s to %s, falling back to full metadata scan: %v", metadataMirror.LastCommit, currentCommit, err)
		return allMetadataFiles(metadataMirror.MetadataPath)
	}

	var issueFiles, pullFiles []string
	for _, rel := range strings.Split(stdout, "\n") {
		rel = strings.TrimSpace(rel)
		if rel == "" || !strings.HasSuffix(rel, ".json") {
			continue
		}
		fullPath := filepath.Join(metadataMirror.MetadataPath, rel)
		if _, err := os.Stat(fullPath); err != nil {
			continue
		}
		switch {
		case strings.HasPrefix(rel, "issues/"):
			issueFiles = append(issueFiles, fullPath)
		case strings.HasPrefix(rel, "pulls/"):
			pullFiles = append(pullFiles, fullPath)
		}
	}
	sortMetadataFiles(issueFiles)
	sortMetadataFiles(pullFiles)
	return issueFiles, pullFiles, nil
}

func allMetadataFiles(metadataPath string) ([]string, []string, error) {
	issueFiles, err := filepath.Glob(filepath.Join(metadataPath, "issues", "*.json"))
	if err != nil {
		return nil, nil, err
	}
	pullFiles, err := filepath.Glob(filepath.Join(metadataPath, "pulls", "*.json"))
	if err != nil {
		return nil, nil, err
	}
	sortMetadataFiles(issueFiles)
	sortMetadataFiles(pullFiles)
	return issueFiles, pullFiles, nil
}

func sortMetadataFiles(files []string) {
	sort.Slice(files, func(i, j int) bool {
		left := metadataFileNumber(files[i])
		right := metadataFileNumber(files[j])
		if left == right {
			return files[i] < files[j]
		}
		return left < right
	})
}

func metadataFileNumber(filename string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSuffix(filepath.Base(filename), filepath.Ext(filename)), 10, 64)
	return n
}

func readLastBackupUnix(metadataPath string) (timeutil.TimeStamp, error) {
	bs, err := os.ReadFile(filepath.Join(metadataPath, "state.json"))
	if err != nil {
		return 0, fmt.Errorf("read metadata state: %w", err)
	}
	var state backupState
	if err := json.Unmarshal(bs, &state); err != nil {
		return 0, fmt.Errorf("parse metadata state: %w", err)
	}
	if state.LastBackup == "" {
		return 0, nil
	}
	lastBackup, err := time.Parse(time.RFC3339Nano, state.LastBackup)
	if err != nil {
		return 0, fmt.Errorf("parse last metadata backup time: %w", err)
	}
	return timeutil.TimeStamp(lastBackup.Unix()), nil
}

func newImporter(ctx context.Context, repo *repo_model.Repository) (*importer, error) {
	labels, err := issues_model.GetLabelsByRepoID(ctx, repo.ID, "", db.ListOptions{})
	if err != nil {
		return nil, err
	}
	labelMap := make(map[string]*issues_model.Label, len(labels))
	for _, repoLabel := range labels {
		labelMap[repoLabel.Name] = repoLabel
	}
	return &importer{repo: repo, labels: labelMap}, nil
}

func (imp *importer) importIssueFile(ctx context.Context, filename string) error {
	var data issueFile
	if err := readJSON(filename, &data); err != nil {
		return err
	}
	issue, err := imp.upsertIssue(ctx, data.Issue)
	if err != nil {
		return err
	}
	return imp.importIssueComments(ctx, issue, data.Events)
}

func (imp *importer) importPullFile(ctx context.Context, filename string) error {
	var data pullFile
	if err := readJSON(filename, &data); err != nil {
		return err
	}
	issue, pr, err := imp.upsertPull(ctx, data.Pull)
	if err != nil {
		return err
	}
	if err := imp.importIssueComments(ctx, issue, data.Events); err != nil {
		return err
	}
	return imp.importReviews(ctx, issue, pr, data.Events, data.Comments)
}

func readJSON(filename string, v any) error {
	bs, err := os.ReadFile(filename)
	if err != nil {
		return err
	}
	return json.Unmarshal(bs, v)
}

func (imp *importer) upsertIssue(ctx context.Context, source githubIssue) (*issues_model.Issue, error) {
	labels, err := imp.ensureLabels(ctx, source.Labels)
	if err != nil {
		return nil, err
	}

	if sourceMap, err := getSourceMap(ctx, imp.repo.ID, sourceKindIssue, source.ID); err != nil {
		return nil, err
	} else if sourceMap != nil {
		issue, err := issues_model.GetIssueByID(ctx, sourceMap.LocalID)
		if err != nil {
			return nil, err
		}
		return issue, imp.updateIssue(ctx, issue, source, labels)
	}

	issue, err := issues_model.GetIssueByIndex(ctx, imp.repo.ID, source.Number)
	if err == nil {
		if err := recordSourceMap(ctx, imp.repo.ID, sourceKindIssue, source.ID, issue.ID, source.Number); err != nil {
			return nil, err
		}
		return issue, imp.updateIssue(ctx, issue, source, labels)
	} else if !issues_model.IsErrIssueNotExist(err) {
		return nil, err
	}

	issue = &issues_model.Issue{
		RepoID:      imp.repo.ID,
		Repo:        imp.repo,
		Index:       source.Number,
		Title:       util.TruncateRunes(source.Title, 255),
		Content:     source.Body,
		IsClosed:    source.State == "closed",
		IsLocked:    source.Locked,
		Labels:      labels,
		PosterID:    user_model.GhostUserID,
		CreatedUnix: timeutil.TimeStamp(source.CreatedAt.Unix()),
		UpdatedUnix: timeutil.TimeStamp(source.UpdatedAt.Unix()),
	}
	applyExternalUser(source.User, &issue.PosterID, &issue.OriginalAuthor, &issue.OriginalAuthorID)
	if source.ClosedAt != nil {
		issue.ClosedUnix = timeutil.TimeStamp(source.ClosedAt.Unix())
	}
	if err := issues_model.InsertIssues(ctx, issue); err != nil {
		return nil, err
	}
	return issue, recordSourceMap(ctx, imp.repo.ID, sourceKindIssue, source.ID, issue.ID, source.Number)
}

func (imp *importer) updateIssue(ctx context.Context, issue *issues_model.Issue, source githubIssue, labels []*issues_model.Label) error {
	issue.Title = util.TruncateRunes(source.Title, 255)
	issue.Content = source.Body
	issue.IsClosed = source.State == "closed"
	issue.IsLocked = source.Locked
	issue.UpdatedUnix = timeutil.TimeStamp(source.UpdatedAt.Unix())
	issue.ClosedUnix = 0
	if source.ClosedAt != nil {
		issue.ClosedUnix = timeutil.TimeStamp(source.ClosedAt.Unix())
	}
	_, err := db.GetEngine(ctx).ID(issue.ID).NoAutoTime().
		Cols("name", "content", "is_closed", "is_locked", "updated_unix", "closed_unix").
		Update(issue)
	if err != nil {
		return err
	}
	return replaceIssueLabels(ctx, issue.ID, labels)
}

func (imp *importer) upsertPull(ctx context.Context, source githubPull) (*issues_model.Issue, *issues_model.PullRequest, error) {
	labels, err := imp.ensureLabels(ctx, source.Labels)
	if err != nil {
		return nil, nil, err
	}

	pr, err := issues_model.GetPullRequestByIndex(ctx, imp.repo.ID, source.Number)
	if err == nil {
		if err := recordSourceMap(ctx, imp.repo.ID, sourceKindPull, source.ID, pr.ID, source.Number); err != nil {
			return nil, nil, err
		}
		return pr.Issue, pr, imp.updatePull(ctx, pr, source, labels)
	} else if !issues_model.IsErrPullRequestNotExist(err) {
		return nil, nil, err
	}

	title := source.Title
	if source.Draft && !issues_model.HasWorkInProgressPrefix(title) {
		title = fmt.Sprintf("%s %s", setting.Repository.PullRequest.WorkInProgressPrefixes[0], title)
	}

	issue := &issues_model.Issue{
		RepoID:      imp.repo.ID,
		Repo:        imp.repo,
		Index:       source.Number,
		Title:       util.TruncateRunes(title, 255),
		Content:     source.Body,
		IsPull:      true,
		IsClosed:    source.State == "closed",
		IsLocked:    source.Locked,
		Labels:      labels,
		PosterID:    user_model.GhostUserID,
		CreatedUnix: timeutil.TimeStamp(source.CreatedAt.Unix()),
		UpdatedUnix: timeutil.TimeStamp(source.UpdatedAt.Unix()),
	}
	applyExternalUser(source.User, &issue.PosterID, &issue.OriginalAuthor, &issue.OriginalAuthorID)
	if source.ClosedAt != nil {
		issue.ClosedUnix = timeutil.TimeStamp(source.ClosedAt.Unix())
	}

	pr = &issues_model.PullRequest{
		Type:           issues_model.PullRequestGitea,
		Status:         issues_model.PullRequestStatusChecking,
		Index:          source.Number,
		HeadRepoID:     imp.repo.ID,
		HeadBranch:     sanitizeBranch(source.Head.Ref),
		BaseRepoID:     imp.repo.ID,
		BaseBranch:     sanitizeBranch(source.Base.Ref),
		MergeBase:      source.Base.SHA,
		HasMerged:      source.Merged,
		MergedCommitID: source.MergeCommitSHA,
		Flow:           issues_model.PullRequestFlowAGit,
		Issue:          issue,
	}
	if source.Merged && source.MergedAt != nil {
		pr.MergedUnix = timeutil.TimeStamp(source.MergedAt.Unix())
		pr.MergerID = user_model.GhostUserID
	}
	if err := updatePullRef(ctx, imp.repo, source.Number, source.Head.SHA); err != nil {
		log.Warn("Unable to update pull ref for %s#%d: %v", imp.repo.FullName(), source.Number, err)
	}
	if err := issues_model.InsertPullRequests(ctx, pr); err != nil {
		return nil, nil, err
	}
	return issue, pr, recordSourceMap(ctx, imp.repo.ID, sourceKindPull, source.ID, pr.ID, source.Number)
}

func (imp *importer) updatePull(ctx context.Context, pr *issues_model.PullRequest, source githubPull, labels []*issues_model.Label) error {
	issue := pr.Issue
	title := source.Title
	if source.Draft && !issues_model.HasWorkInProgressPrefix(title) {
		title = fmt.Sprintf("%s %s", setting.Repository.PullRequest.WorkInProgressPrefixes[0], title)
	}
	issue.Title = util.TruncateRunes(title, 255)
	issue.Content = source.Body
	issue.IsClosed = source.State == "closed"
	issue.IsLocked = source.Locked
	issue.UpdatedUnix = timeutil.TimeStamp(source.UpdatedAt.Unix())
	issue.ClosedUnix = 0
	if source.ClosedAt != nil {
		issue.ClosedUnix = timeutil.TimeStamp(source.ClosedAt.Unix())
	}
	if _, err := db.GetEngine(ctx).ID(issue.ID).NoAutoTime().
		Cols("name", "content", "is_closed", "is_locked", "updated_unix", "closed_unix").
		Update(issue); err != nil {
		return err
	}
	if err := replaceIssueLabels(ctx, issue.ID, labels); err != nil {
		return err
	}

	pr.HeadBranch = sanitizeBranch(source.Head.Ref)
	pr.BaseBranch = sanitizeBranch(source.Base.Ref)
	pr.Type = issues_model.PullRequestGitea
	pr.MergeBase = source.Base.SHA
	pr.HasMerged = source.Merged
	pr.MergedCommitID = source.MergeCommitSHA
	pr.Flow = issues_model.PullRequestFlowAGit
	pr.MergedUnix = 0
	pr.MergerID = 0
	if source.Merged && source.MergedAt != nil {
		pr.MergedUnix = timeutil.TimeStamp(source.MergedAt.Unix())
		pr.MergerID = user_model.GhostUserID
	}
	if _, err := db.GetEngine(ctx).ID(pr.ID).NoAutoTime().
		Cols("head_branch", "base_branch", "type", "merge_base", "has_merged", "merged_commit_id", "flow", "merged_unix", "merger_id").
		Update(pr); err != nil {
		return err
	}
	if err := updatePullRef(ctx, imp.repo, source.Number, source.Head.SHA); err != nil {
		log.Warn("Unable to update pull ref for %s#%d: %v", imp.repo.FullName(), source.Number, err)
	}
	return nil
}

func (imp *importer) ensureLabels(ctx context.Context, labels []githubLabel) ([]*issues_model.Label, error) {
	result := make([]*issues_model.Label, 0, len(labels))
	for _, source := range labels {
		if source.Name == "" {
			continue
		}
		if existing, ok := imp.labels[source.Name]; ok {
			result = append(result, existing)
			continue
		}
		color, err := label.NormalizeColor(source.Color)
		if err != nil {
			color = "#ffffff"
		}
		repoLabel := &issues_model.Label{
			RepoID:      imp.repo.ID,
			Name:        source.Name,
			Color:       color,
			Description: source.Description,
		}
		if err := issues_model.NewLabel(ctx, repoLabel); err != nil {
			return nil, err
		}
		imp.labels[repoLabel.Name] = repoLabel
		result = append(result, repoLabel)
	}
	return result, nil
}

func (imp *importer) importIssueComments(ctx context.Context, issue *issues_model.Issue, events []githubEvent) error {
	comments := make([]*issues_model.Comment, 0)
	sourceIDs := make([]int64, 0)
	for _, event := range events {
		if event.Event != "commented" || event.ID == 0 {
			continue
		}
		if sourceMap, err := getSourceMap(ctx, imp.repo.ID, sourceKindIssueComment, event.ID); err != nil {
			return err
		} else if sourceMap != nil {
			continue
		}

		created := eventTime(event.CreatedAt, nil)
		updated := eventTime(event.UpdatedAt, &created)
		comment := &issues_model.Comment{
			Type:        issues_model.CommentTypeComment,
			IssueID:     issue.ID,
			PosterID:    user_model.GhostUserID,
			Content:     event.Body,
			CreatedUnix: timeutil.TimeStamp(created.Unix()),
			UpdatedUnix: timeutil.TimeStamp(updated.Unix()),
		}
		applyExternalUser(eventUser(event), &comment.PosterID, &comment.OriginalAuthor, &comment.OriginalAuthorID)
		comments = append(comments, comment)
		sourceIDs = append(sourceIDs, event.ID)
	}
	if len(comments) == 0 {
		return nil
	}
	if err := issues_model.InsertIssueComments(ctx, comments); err != nil {
		return err
	}
	for i, comment := range comments {
		if err := recordSourceMap(ctx, imp.repo.ID, sourceKindIssueComment, sourceIDs[i], comment.ID, issue.Index); err != nil {
			return err
		}
	}
	return nil
}

func (imp *importer) importReviews(ctx context.Context, issue *issues_model.Issue, pr *issues_model.PullRequest, events []githubEvent, comments []githubReviewComment) error {
	commentsByReview := make(map[int64][]githubReviewComment)
	commentsWithoutReview := make(map[int64][]githubReviewComment)
	for _, comment := range comments {
		if comment.ID == 0 {
			continue
		}
		if comment.PullRequestReviewID == 0 {
			userID := int64(0)
			if comment.User != nil {
				userID = comment.User.ID
			}
			commentsWithoutReview[userID] = append(commentsWithoutReview[userID], comment)
			continue
		}
		commentsByReview[comment.PullRequestReviewID] = append(commentsByReview[comment.PullRequestReviewID], comment)
	}

	seenReviews := make(map[int64]bool)
	for _, event := range events {
		if event.Event != "reviewed" || event.ID == 0 {
			continue
		}
		seenReviews[event.ID] = true
		if err := imp.importReview(ctx, issue, event, commentsByReview[event.ID]); err != nil {
			return err
		}
	}

	for reviewID, reviewComments := range commentsByReview {
		if seenReviews[reviewID] {
			continue
		}
		if err := imp.importSyntheticReview(ctx, issue, pr, reviewID, reviewComments); err != nil {
			return err
		}
	}
	for _, reviewComments := range commentsWithoutReview {
		if len(reviewComments) == 0 {
			continue
		}
		if err := imp.importSyntheticReview(ctx, issue, pr, -reviewComments[0].ID, reviewComments); err != nil {
			return err
		}
	}
	return nil
}

func (imp *importer) importReview(ctx context.Context, issue *issues_model.Issue, event githubEvent, comments []githubReviewComment) error {
	if sourceMap, err := getSourceMap(ctx, imp.repo.ID, sourceKindReview, event.ID); err != nil {
		return err
	} else if sourceMap != nil {
		return imp.importReviewComments(ctx, issue, sourceMap.LocalID, comments)
	}

	created := eventTime(event.SubmittedAt, event.CreatedAt)
	review := &issues_model.Review{
		Type:        reviewType(event.State),
		IssueID:     issue.ID,
		Content:     event.Body,
		ReviewerID:  user_model.GhostUserID,
		CommitID:    event.CommitID,
		CreatedUnix: timeutil.TimeStamp(created.Unix()),
		UpdatedUnix: timeutil.TimeStamp(created.Unix()),
	}
	applyExternalUser(event.User, &review.ReviewerID, &review.OriginalAuthor, &review.OriginalAuthorID)
	commentSourceIDs := make([]int64, 0, len(comments))
	for _, comment := range comments {
		if sourceMap, err := getSourceMap(ctx, imp.repo.ID, sourceKindReviewComment, comment.ID); err != nil {
			return err
		} else if sourceMap == nil {
			review.Comments = append(review.Comments, reviewComment(issue, review.ID, comment))
			commentSourceIDs = append(commentSourceIDs, comment.ID)
		}
	}
	if err := issues_model.InsertReviews(ctx, []*issues_model.Review{review}); err != nil {
		return err
	}
	if err := recordSourceMap(ctx, imp.repo.ID, sourceKindReview, event.ID, review.ID, issue.Index); err != nil {
		return err
	}
	for i, comment := range review.Comments {
		if err := recordSourceMap(ctx, imp.repo.ID, sourceKindReviewComment, commentSourceIDs[i], comment.ID, issue.Index); err != nil {
			return err
		}
	}
	return nil
}

func (imp *importer) importSyntheticReview(ctx context.Context, issue *issues_model.Issue, pr *issues_model.PullRequest, reviewID int64, comments []githubReviewComment) error {
	if sourceMap, err := getSourceMap(ctx, imp.repo.ID, sourceKindReview, reviewID); err != nil {
		return err
	} else if sourceMap != nil {
		return imp.importReviewComments(ctx, issue, sourceMap.LocalID, comments)
	}

	if len(comments) == 0 {
		return nil
	}
	first := comments[0]
	review := &issues_model.Review{
		Type:        issues_model.ReviewTypeComment,
		IssueID:     issue.ID,
		ReviewerID:  user_model.GhostUserID,
		CommitID:    first.CommitID,
		CreatedUnix: timeutil.TimeStamp(first.CreatedAt.Unix()),
		UpdatedUnix: timeutil.TimeStamp(first.UpdatedAt.Unix()),
	}
	applyExternalUser(first.User, &review.ReviewerID, &review.OriginalAuthor, &review.OriginalAuthorID)
	commentSourceIDs := make([]int64, 0, len(comments))
	for _, comment := range comments {
		if sourceMap, err := getSourceMap(ctx, imp.repo.ID, sourceKindReviewComment, comment.ID); err != nil {
			return err
		} else if sourceMap == nil {
			review.Comments = append(review.Comments, reviewComment(issue, review.ID, comment))
			commentSourceIDs = append(commentSourceIDs, comment.ID)
		}
	}
	if err := issues_model.InsertReviews(ctx, []*issues_model.Review{review}); err != nil {
		return err
	}
	if err := recordSourceMap(ctx, imp.repo.ID, sourceKindReview, reviewID, review.ID, pr.Index); err != nil {
		return err
	}
	for i, comment := range review.Comments {
		if err := recordSourceMap(ctx, imp.repo.ID, sourceKindReviewComment, commentSourceIDs[i], comment.ID, issue.Index); err != nil {
			return err
		}
	}
	return nil
}

func (imp *importer) importReviewComments(ctx context.Context, issue *issues_model.Issue, reviewID int64, comments []githubReviewComment) error {
	newComments := make([]*issues_model.Comment, 0)
	sourceIDs := make([]int64, 0)
	for _, comment := range comments {
		if sourceMap, err := getSourceMap(ctx, imp.repo.ID, sourceKindReviewComment, comment.ID); err != nil {
			return err
		} else if sourceMap != nil {
			continue
		}
		newComments = append(newComments, reviewComment(issue, reviewID, comment))
		sourceIDs = append(sourceIDs, comment.ID)
	}
	if len(newComments) == 0 {
		return nil
	}
	if err := issues_model.InsertIssueComments(ctx, newComments); err != nil {
		return err
	}
	for i, comment := range newComments {
		if err := recordSourceMap(ctx, imp.repo.ID, sourceKindReviewComment, sourceIDs[i], comment.ID, issue.Index); err != nil {
			return err
		}
	}
	return nil
}

func reviewComment(issue *issues_model.Issue, reviewID int64, source githubReviewComment) *issues_model.Comment {
	line := reviewCommentLine(source)
	comment := &issues_model.Comment{
		Type:        issues_model.CommentTypeCode,
		IssueID:     issue.ID,
		ReviewID:    reviewID,
		PosterID:    user_model.GhostUserID,
		Content:     source.Body,
		TreePath:    util.PathJoinRel(source.Path),
		Line:        line,
		Patch:       source.DiffHunk,
		CommitSHA:   firstNonEmpty(source.CommitID, source.OriginalCommitID),
		CreatedUnix: timeutil.TimeStamp(source.CreatedAt.Unix()),
		UpdatedUnix: timeutil.TimeStamp(source.UpdatedAt.Unix()),
	}
	if source.StartLine != nil || source.OriginalStartLine != nil {
		startLine := int64PtrValue(source.StartLine, source.OriginalStartLine)
		if startLine > 0 && abs64(line)-startLine > 0 {
			comment.ExtraLinesCount = abs64(line) - startLine
		}
	}
	applyExternalUser(source.User, &comment.PosterID, &comment.OriginalAuthor, &comment.OriginalAuthorID)
	return comment
}

func reviewCommentLine(source githubReviewComment) int64 {
	line := int64PtrValue(source.Line, source.OriginalLine)
	if line == 0 {
		return 0
	}
	if source.Side == "LEFT" {
		return -line
	}
	return line
}

func replaceIssueLabels(ctx context.Context, issueID int64, labels []*issues_model.Label) error {
	return db.WithTx(ctx, func(ctx context.Context) error {
		if _, err := db.GetEngine(ctx).Where("issue_id = ?", issueID).Delete(new(issues_model.IssueLabel)); err != nil {
			return err
		}
		issueLabels := make([]issues_model.IssueLabel, 0, len(labels))
		for _, label := range labels {
			issueLabels = append(issueLabels, issues_model.IssueLabel{IssueID: issueID, LabelID: label.ID})
		}
		if len(issueLabels) > 0 {
			return db.Insert(ctx, issueLabels)
		}
		return nil
	})
}

func getSourceMap(ctx context.Context, repoID int64, kind string, sourceID int64) (*repo_model.GitHubMetadataMirrorMap, error) {
	if sourceID == 0 {
		return nil, nil
	}
	sourceMap := &repo_model.GitHubMetadataMirrorMap{
		RepoID:     repoID,
		SourceKind: kind,
		SourceID:   sourceID,
	}
	has, err := db.GetEngine(ctx).Get(sourceMap)
	if err != nil {
		return nil, err
	}
	if !has {
		return nil, nil
	}
	return sourceMap, nil
}

func recordSourceMap(ctx context.Context, repoID int64, kind string, sourceID, localID, issueIndex int64) error {
	if sourceID == 0 {
		return nil
	}
	if sourceMap, err := getSourceMap(ctx, repoID, kind, sourceID); err != nil {
		return err
	} else if sourceMap != nil {
		return nil
	}
	return db.Insert(ctx, &repo_model.GitHubMetadataMirrorMap{
		RepoID:     repoID,
		SourceKind: kind,
		SourceID:   sourceID,
		LocalID:    localID,
		IssueIndex: issueIndex,
	})
}

func applyExternalUser(source *githubUser, posterID *int64, originalAuthor *string, originalAuthorID *int64) {
	if source == nil || source.ID == 0 {
		*posterID = user_model.GhostUserID
		return
	}
	*posterID = user_model.GhostUserID
	*originalAuthor = source.Login
	*originalAuthorID = source.ID
}

func eventUser(event githubEvent) *githubUser {
	if event.User != nil {
		return event.User
	}
	return event.Actor
}

func eventTime(primary *time.Time, fallback *time.Time) time.Time {
	if primary != nil && !primary.IsZero() {
		return *primary
	}
	if fallback != nil && !fallback.IsZero() {
		return *fallback
	}
	return time.Now()
}

func reviewType(state string) issues_model.ReviewType {
	switch strings.ToUpper(state) {
	case "APPROVED":
		return issues_model.ReviewTypeApprove
	case "CHANGES_REQUESTED":
		return issues_model.ReviewTypeReject
	case "COMMENTED":
		return issues_model.ReviewTypeComment
	case "PENDING":
		return issues_model.ReviewTypePending
	default:
		return issues_model.ReviewTypeComment
	}
}

func sanitizeBranch(branch string) string {
	if branch == "" {
		return ""
	}
	if git.IsValidRefPattern(branch) {
		return branch
	}
	return git.SanitizeRefPattern(branch)
}

func updatePullRef(ctx context.Context, repo *repo_model.Repository, index int64, sha string) error {
	if sha == "" {
		return nil
	}
	if _, _, err := git.NewCommand(ctx, "rev-list", "--quiet", "-1").
		AddDynamicArguments(sha).
		RunStdString(&git.RunOpts{Dir: repo.RepoPath()}); err != nil {
		return nil
	}
	refName := fmt.Sprintf("%s%d/head", git.PullPrefix, index)
	_, _, err := git.NewCommand(ctx, "update-ref", "--no-deref").
		AddDynamicArguments(refName, sha).
		RunStdString(&git.RunOpts{Dir: repo.RepoPath()})
	return err
}

func int64PtrValue(primary *int, fallback *int) int64 {
	if primary != nil {
		return int64(*primary)
	}
	if fallback != nil {
		return int64(*fallback)
	}
	return 0
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
