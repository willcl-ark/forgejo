// Copyright 2026 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: GPL-3.0-or-later

package forgejo_migrations

import (
	"time"

	"forgejo.org/modules/timeutil"

	"code.forgejo.org/xorm/xorm"
)

func init() {
	registerMigration(&Migration{
		Description: "add GitHub metadata mirror tables",
		Upgrade:     addGitHubMetadataMirrorTables,
	})
}

func addGitHubMetadataMirrorTables(x *xorm.Engine) error {
	type GitHubMetadataMirror struct {
		ID             int64  `xorm:"pk autoincr"`
		RepoID         int64  `xorm:"UNIQUE INDEX"`
		MetadataPath   string `xorm:"TEXT NOT NULL"`
		MetadataURL    string `xorm:"TEXT"`
		Interval       time.Duration
		LastCommit     string `xorm:"VARCHAR(64)"`
		LastBackupUnix timeutil.TimeStamp
		UpdatedUnix    timeutil.TimeStamp `xorm:"INDEX"`
		NextUpdateUnix timeutil.TimeStamp `xorm:"INDEX"`
	}

	type GitHubMetadataMirrorMap struct {
		ID          int64  `xorm:"pk autoincr"`
		RepoID      int64  `xorm:"UNIQUE(repo_source) INDEX"`
		SourceKind  string `xorm:"VARCHAR(32) UNIQUE(repo_source) INDEX"`
		SourceID    int64  `xorm:"UNIQUE(repo_source) INDEX"`
		LocalID     int64
		IssueIndex  int64
		CreatedUnix timeutil.TimeStamp `xorm:"INDEX CREATED"`
	}

	_, err := x.SyncWithOptions(
		xorm.SyncOptions{IgnoreDropIndices: true},
		new(GitHubMetadataMirror),
		new(GitHubMetadataMirrorMap),
	)
	return err
}
