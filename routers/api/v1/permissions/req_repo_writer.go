// Copyright 2026 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: GPL-3.0-or-later

package permissions

import (
	"net/http"
	"slices"

	repo_model "forgejo.org/models/repo"
	"forgejo.org/models/unit"
)

func ReqRepoWriter(ctx Context, unitTypes []unit.Type) {
	if !slices.ContainsFunc(unitTypes, func(unitType unit.Type) bool {
		return ctx.GetRepository().UnitEnabled(ctx.GetContext(), unitType)
	}) {
		ctx.NotFound()
		return
	}
	if slices.ContainsFunc(unitTypes, isIssueOrPullUnit) {
		isGitHubMetadataMirror, err := repo_model.IsGitHubMetadataMirror(ctx.GetContext(), ctx.GetRepository().ID)
		if err != nil {
			ctx.Error(http.StatusInternalServerError, "IsGitHubMetadataMirror", err)
			return
		}
		if isGitHubMetadataMirror {
			ctx.Error(http.StatusForbidden, "reqRepoWriter", "GitHub metadata mirrors are read-only for issues and pulls")
			return
		}
	}
	if !IsUserRepoWriter(ctx, unitTypes) && !IsUserRepoAdmin(ctx) && !IsUserSiteAdmin(ctx) {
		ctx.Error(http.StatusForbidden, "reqRepoWriter", "user should have a permission to write to a repo")
		return
	}
}

func isIssueOrPullUnit(unitType unit.Type) bool {
	return unitType == unit.TypeIssues || unitType == unit.TypePullRequests
}
