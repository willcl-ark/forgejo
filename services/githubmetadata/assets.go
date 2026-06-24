// Copyright 2026 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package githubmetadata

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"

	"forgejo.org/models/db"
	issues_model "forgejo.org/models/issues"
	repo_model "forgejo.org/models/repo"
	"forgejo.org/modules/log"
	"forgejo.org/modules/timeutil"
	attachment_service "forgejo.org/services/attachment"
	"forgejo.org/services/migrations"
)

var githubAssetURLPattern = regexp.MustCompile(`https://(?:user-images\.githubusercontent\.com|private-user-images\.githubusercontent\.com|github\.com/[^\s\]\[()"'<>]+/(?:assets|user-attachments/assets)/[^\s\]\[()"'<>]+|github-production-user-asset-[A-Za-z0-9-]+\.s3\.amazonaws\.com)[^\s\]\[()"'<>]*`)

var imageExtensions = map[string]bool{
	".apng": true,
	".avif": true,
	".bmp":  true,
	".gif":  true,
	".jpg":  true,
	".jpeg": true,
	".jxl":  true,
	".png":  true,
	".svg":  true,
	".webp": true,
}

type assetTarget struct {
	RepoID      int64
	IssueID     int64
	CommentID   int64
	ReleaseID   int64
	UploaderID  int64
	CreatedUnix int64
}

type assetDownloadError struct {
	err error
}

func (e assetDownloadError) Error() string {
	return e.err.Error()
}

func (e assetDownloadError) Unwrap() error {
	return e.err
}

var downloadGitHubAsset = downloadGitHubAssetURL

func (imp *importer) localizeIssueContent(ctx context.Context, issue *issues_model.Issue, content string) error {
	localized, err := imp.localizeContentAssets(ctx, content, assetTarget{
		RepoID:      issue.RepoID,
		IssueID:     issue.ID,
		UploaderID:  issue.PosterID,
		CreatedUnix: int64(issue.CreatedUnix),
	})
	if err != nil {
		return err
	}
	if localized == issue.Content {
		return nil
	}
	issue.Content = localized
	_, err = db.GetEngine(ctx).ID(issue.ID).NoAutoTime().Cols("content").Update(issue)
	return err
}

func (imp *importer) localizeCommentContent(ctx context.Context, comment *issues_model.Comment) error {
	localized, err := imp.localizeContentAssets(ctx, comment.Content, assetTarget{
		RepoID:      imp.repo.ID,
		IssueID:     comment.IssueID,
		CommentID:   comment.ID,
		UploaderID:  comment.PosterID,
		CreatedUnix: int64(comment.CreatedUnix),
	})
	if err != nil {
		return err
	}
	if localized == comment.Content {
		return nil
	}
	comment.Content = localized
	_, err = db.GetEngine(ctx).ID(comment.ID).NoAutoTime().Cols("content").Update(comment)
	return err
}

func (imp *importer) localizeReviewContent(ctx context.Context, issue *issues_model.Issue, review *issues_model.Review) error {
	comment := &issues_model.Comment{}
	hasComment, err := db.GetEngine(ctx).
		Where("review_id = ?", review.ID).
		And("type = ?", issues_model.CommentTypeReview).
		Get(comment)
	if err != nil {
		return err
	}

	target := assetTarget{
		RepoID:      imp.repo.ID,
		IssueID:     issue.ID,
		UploaderID:  review.ReviewerID,
		CreatedUnix: int64(review.CreatedUnix),
	}
	if hasComment {
		target.CommentID = comment.ID
	}

	localized, err := imp.localizeContentAssets(ctx, review.Content, target)
	if err != nil {
		return err
	}
	if localized == review.Content {
		return nil
	}

	review.Content = localized
	if _, err := db.GetEngine(ctx).ID(review.ID).NoAutoTime().Cols("content").Update(review); err != nil {
		return err
	}
	if hasComment {
		comment.Content = localized
		comment.UpdatedUnix = review.UpdatedUnix
		_, err = db.GetEngine(ctx).ID(comment.ID).NoAutoTime().Cols("content").Update(comment)
	}
	return err
}

func (imp *importer) localizeContentAssets(ctx context.Context, content string, target assetTarget) (string, error) {
	if content == "" || !githubAssetURLPattern.MatchString(content) {
		return content, nil
	}

	replacements := make(map[string]string)
	for _, rawURL := range githubAssetURLPattern.FindAllString(content, -1) {
		rawURL = strings.TrimRight(rawURL, ".,;:")
		if rawURL == "" {
			continue
		}
		if _, ok := replacements[rawURL]; ok {
			continue
		}

		localURL, err := imp.localizeAsset(ctx, rawURL, target)
		if err != nil {
			var downloadErr assetDownloadError
			if errors.As(err, &downloadErr) {
				log.Warn("Unable to download GitHub asset %q for repo_id=%d issue_id=%d comment_id=%d release_id=%d: %v", rawURL, target.RepoID, target.IssueID, target.CommentID, target.ReleaseID, err)
				continue
			}
			return "", err
		}
		if localURL != "" {
			replacements[rawURL] = localURL
		}
	}

	for rawURL, localURL := range replacements {
		content = strings.ReplaceAll(content, rawURL, localURL)
	}
	return content, nil
}

func (imp *importer) localizeAsset(ctx context.Context, rawURL string, target assetTarget) (string, error) {
	if target.RepoID == 0 {
		target.RepoID = imp.repo.ID
	}
	if target.UploaderID == 0 {
		target.UploaderID = imp.repo.OwnerID
	}

	existing, err := findExistingLocalizedAsset(ctx, target, rawURL)
	if err != nil {
		return "", err
	}
	if existing != nil {
		return existing.DownloadURL(), nil
	}

	body, contentType, finalURL, size, err := downloadGitHubAsset(ctx, rawURL)
	if err != nil {
		return "", assetDownloadError{err: err}
	}
	defer body.Close()

	if !isGitHubAssetImage(rawURL, contentType) {
		return "", nil
	}

	name := githubAssetAttachmentName(rawURL, finalURL, contentType)
	existing, err = findLocalizedAsset(ctx, target, name)
	if err != nil {
		return "", err
	}
	if existing != nil {
		return existing.DownloadURL(), nil
	}

	attach := &repo_model.Attachment{
		RepoID:      target.RepoID,
		IssueID:     target.IssueID,
		CommentID:   target.CommentID,
		ReleaseID:   target.ReleaseID,
		UploaderID:  target.UploaderID,
		Name:        name,
		CreatedUnix: timeutil.TimeStamp(target.CreatedUnix),
		NoAutoTime:  target.CreatedUnix != 0,
	}
	created, err := attachment_service.NewAttachment(ctx, attach, body, size)
	if err != nil {
		return "", err
	}
	return created.DownloadURL(), nil
}

func downloadGitHubAssetURL(ctx context.Context, rawURL string) (io.ReadCloser, string, string, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", "", 0, err
	}
	req.Header.Set("User-Agent", "Forgejo GitHub metadata mirror")

	resp, err := migrations.NewMigrationHTTPClient().Do(req)
	if err != nil {
		return nil, "", "", 0, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = resp.Body.Close()
		return nil, "", "", 0, fmt.Errorf("unexpected status %s", resp.Status)
	}
	return resp.Body, resp.Header.Get("Content-Type"), resp.Request.URL.String(), resp.ContentLength, nil
}

func isGitHubAssetImage(rawURL, contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err == nil && strings.HasPrefix(mediaType, "image/") {
		return true
	}
	return imageExtensions[githubAssetExtension(rawURL)]
}

func githubAssetAttachmentName(rawURL, finalURL, contentType string) string {
	canonicalURL := canonicalGitHubAssetURL(rawURL)
	ext := githubAssetExtension(canonicalURL)
	if ext == "" {
		ext = githubAssetExtension(finalURL)
	}
	if ext == "" {
		mediaType, _, err := mime.ParseMediaType(contentType)
		if err == nil {
			exts, _ := mime.ExtensionsByType(mediaType)
			if len(exts) > 0 {
				ext = exts[0]
			}
		}
	}
	if ext == ".jpe" {
		ext = ".jpg"
	}
	if ext == "" {
		ext = ".img"
	}

	return githubAssetAttachmentNameWithExtension(canonicalURL, ext)
}

func githubAssetAttachmentNameWithExtension(canonicalURL, ext string) string {
	parts := githubAssetNameParts(canonicalURL)
	if len(parts) > 0 {
		if lastExt := strings.ToLower(filepath.Ext(parts[len(parts)-1])); imageExtensions[lastExt] {
			parts[len(parts)-1] = strings.TrimSuffix(parts[len(parts)-1], lastExt)
		}
	}

	stem := "github-asset-" + slugGitHubAssetName(strings.Join(parts, "-"))
	if stem == "github-asset-" {
		stem = "github-asset"
	}

	const maxNameLength = 180
	if len(stem)+len(ext) > maxNameLength {
		sum := sha256.Sum256([]byte(canonicalURL))
		digest := hex.EncodeToString(sum[:])[:12]
		maxStemLength := maxNameLength - len(ext) - len(digest) - 1
		stem = strings.TrimRight(stem[:maxStemLength], "-") + "-" + digest
	}
	return stem + ext
}

func canonicalGitHubAssetURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	u.RawQuery = ""
	u.Fragment = ""
	u.Host = strings.ToLower(u.Host)
	u.Path, _ = url.PathUnescape(u.Path)
	return u.String()
}

func githubAssetNameParts(rawURL string) []string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return []string{"asset"}
	}
	parts := strings.FieldsFunc(u.EscapedPath(), func(r rune) bool { return r == '/' })
	for i := range parts {
		parts[i], _ = url.PathUnescape(parts[i])
	}

	switch {
	case u.Host == "user-images.githubusercontent.com":
		return append([]string{"user-images"}, parts...)
	case u.Host == "private-user-images.githubusercontent.com":
		return append([]string{"private-user-images"}, parts...)
	case u.Host == "github.com":
		for i, part := range parts {
			if part == "user-attachments" {
				return parts[i:]
			}
		}
		return parts
	case strings.HasPrefix(u.Host, "github-production-user-asset-") && strings.HasSuffix(u.Host, ".s3.amazonaws.com"):
		return parts
	default:
		return append([]string{u.Host}, parts...)
	}
}

func githubAssetExtension(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	ext := strings.ToLower(filepath.Ext(u.Path))
	if imageExtensions[ext] {
		return ext
	}
	return ""
}

func slugGitHubAssetName(name string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
			lastDash = r == '-'
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), ".-_")
}

func findExistingLocalizedAsset(ctx context.Context, target assetTarget, rawURL string) (*repo_model.Attachment, error) {
	canonicalURL := canonicalGitHubAssetURL(rawURL)
	if ext := githubAssetExtension(canonicalURL); ext != "" {
		return findLocalizedAsset(ctx, target, githubAssetAttachmentNameWithExtension(canonicalURL, ext))
	}

	candidateNames := make([]string, 0, len(imageExtensions)+1)
	for _, ext := range []string{".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg", ".apng", ".avif", ".bmp", ".jxl", ".img"} {
		candidateNames = append(candidateNames, githubAssetAttachmentNameWithExtension(canonicalURL, ext))
	}
	return findLocalizedAsset(ctx, target, candidateNames...)
}

func findLocalizedAsset(ctx context.Context, target assetTarget, names ...string) (*repo_model.Attachment, error) {
	if len(names) == 0 {
		return nil, nil
	}
	attachments := make([]*repo_model.Attachment, 0, 1)
	if err := db.GetEngine(ctx).
		Where("repo_id = ?", target.RepoID).
		And("issue_id = ?", target.IssueID).
		And("comment_id = ?", target.CommentID).
		And("release_id = ?", target.ReleaseID).
		In("name", names).
		And("COALESCE(external_url, '') = ''").
		Asc("id").
		Limit(1).
		Find(&attachments); err != nil {
		return nil, err
	}
	if len(attachments) == 0 {
		return nil, nil
	}
	return attachments[0], nil
}
