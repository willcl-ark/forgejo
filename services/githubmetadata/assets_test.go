// Copyright 2026 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package githubmetadata

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGitHubAssetAttachmentName(t *testing.T) {
	tests := []struct {
		name        string
		rawURL      string
		contentType string
		want        string
	}{
		{
			name:        "user images keeps source identifiers",
			rawURL:      "https://user-images.githubusercontent.com/1/2-example.png?raw=1",
			contentType: "image/png",
			want:        "github-asset-user-images-1-2-example.png",
		},
		{
			name:        "github user attachments without extension uses content type",
			rawURL:      "https://github.com/user-attachments/assets/601709509-77b92691-9dcd-4bd9-b986-3b4e704d88cc",
			contentType: "image/png",
			want:        "github-asset-user-attachments-assets-601709509-77b92691-9dcd-4bd9-b986-3b4e704d88cc.png",
		},
		{
			name:        "s3 asset keeps path and strips query",
			rawURL:      "https://github-production-user-asset-6210df.s3.amazonaws.com/1841944/601709509-77b92691.png?X-Amz-Signature=abc",
			contentType: "image/png",
			want:        "github-asset-1841944-601709509-77b92691.png",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, githubAssetAttachmentName(tt.rawURL, tt.rawURL, tt.contentType))
		})
	}
}

func TestCanonicalGitHubAssetURL(t *testing.T) {
	assert.Equal(t,
		"https://user-images.githubusercontent.com/1/2%20example.png",
		canonicalGitHubAssetURL("https://user-images.githubusercontent.com/1/2%20example.png?raw=1#fragment"),
	)
}

func TestGitHubAssetURLPattern(t *testing.T) {
	content := `![old](https://user-images.githubusercontent.com/1/2.png) ![new](https://github.com/user-attachments/assets/abc123) [raw](https://raw.githubusercontent.com/bitcoin/bitcoin/master/doc/README.md) [non-image](https://example.com/no.png)`
	matches := githubAssetURLPattern.FindAllString(content, -1)
	assert.Equal(t, []string{
		"https://user-images.githubusercontent.com/1/2.png",
		"https://github.com/user-attachments/assets/abc123",
	}, matches)
}
