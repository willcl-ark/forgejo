// Copyright 2026 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repo

import (
	"context"
	"time"

	"forgejo.org/models/db"
	"forgejo.org/modules/timeutil"
)

// GitHubMetadataMirror stores the local JSON backup repository used to mirror
// issue and pull request metadata for a pull-mirrored repository.
type GitHubMetadataMirror struct {
	ID           int64       `xorm:"pk autoincr"`
	RepoID       int64       `xorm:"UNIQUE INDEX"`
	Repo         *Repository `xorm:"-"`
	MetadataPath string      `xorm:"TEXT NOT NULL"`
	MetadataURL  string      `xorm:"TEXT"`

	Interval time.Duration

	LastCommit     string `xorm:"VARCHAR(64)"`
	LastBackupUnix timeutil.TimeStamp

	UpdatedUnix    timeutil.TimeStamp `xorm:"INDEX"`
	NextUpdateUnix timeutil.TimeStamp `xorm:"INDEX"`
}

// GitHubMetadataMirrorMap maps GitHub API source object IDs to local Forgejo IDs.
type GitHubMetadataMirrorMap struct {
	ID          int64  `xorm:"pk autoincr"`
	RepoID      int64  `xorm:"UNIQUE(repo_source) INDEX"`
	SourceKind  string `xorm:"VARCHAR(32) UNIQUE(repo_source) INDEX"`
	SourceID    int64  `xorm:"UNIQUE(repo_source) INDEX"`
	LocalID     int64
	IssueIndex  int64
	CreatedUnix timeutil.TimeStamp `xorm:"INDEX CREATED"`
}

func init() {
	db.RegisterModel(new(GitHubMetadataMirror))
	db.RegisterModel(new(GitHubMetadataMirrorMap))
}

// BeforeInsert will be invoked by XORM before inserting a record.
func (m *GitHubMetadataMirror) BeforeInsert() {
	if m.UpdatedUnix.IsZero() {
		m.UpdatedUnix = timeutil.TimeStampNow()
	}
}

// ScheduleNextUpdate calculates and sets the next metadata update time.
func (m *GitHubMetadataMirror) ScheduleNextUpdate() {
	if m.Interval != 0 {
		m.NextUpdateUnix = timeutil.TimeStampNow().AddDuration(m.Interval)
	} else {
		m.NextUpdateUnix = 0
	}
}

// InsertGitHubMetadataMirror inserts a GitHub metadata mirror.
func InsertGitHubMetadataMirror(ctx context.Context, m *GitHubMetadataMirror) error {
	return db.Insert(ctx, m)
}

// GetGitHubMetadataMirrorByRepoID returns metadata mirror information of a repository.
func GetGitHubMetadataMirrorByRepoID(ctx context.Context, repoID int64) (*GitHubMetadataMirror, error) {
	m := &GitHubMetadataMirror{RepoID: repoID}
	has, err := db.GetEngine(ctx).Get(m)
	if err != nil {
		return nil, err
	} else if !has {
		return nil, ErrMirrorNotExist
	}
	return m, nil
}

// IsGitHubMetadataMirror returns true if a repository has metadata mirror config.
func IsGitHubMetadataMirror(ctx context.Context, repoID int64) (bool, error) {
	return db.GetEngine(ctx).Exist(&GitHubMetadataMirror{RepoID: repoID})
}

// UpdateGitHubMetadataMirror updates the metadata mirror.
func UpdateGitHubMetadataMirror(ctx context.Context, m *GitHubMetadataMirror) error {
	_, err := db.GetEngine(ctx).ID(m.ID).AllCols().Update(m)
	return err
}

// GitHubMetadataMirrorsIterate iterates due metadata mirrors.
func GitHubMetadataMirrorsIterate(ctx context.Context, limit int, f func(idx int, bean any) error) error {
	sess := db.GetEngine(ctx).
		Where("next_update_unix<=?", time.Now().Unix()).
		And("next_update_unix!=0").
		OrderBy("updated_unix ASC")
	if limit > 0 {
		sess = sess.Limit(limit)
	}
	return sess.Iterate(new(GitHubMetadataMirror), f)
}
