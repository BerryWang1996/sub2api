//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestForkVersionComparison(t *testing.T) {
	for _, testCase := range []struct {
		current, latest string
		want            int
	}{
		{"v0.2.19-barry.2", "v0.2.19-barry.10", -1},
		{"0.2.19-barry.10", "v0.2.19-barry.2", 1},
		{"v0.2.19-barry.2", "0.2.19-barry.2", 0},
		{"v0.2.19-barry.99", "v0.2.20-barry.1", -1},
		{"barry-20261028.2", "barry-20261028.10", -1},
		{"barry-20261028.99", "barry-20261105.1", -1},
		{"barry-20261028.1", "v0.2.19-barry.2", -1},
		{"0.2.19-barry.abcdef123456", "v0.2.19-barry.2", -1},
		{"0.2.20-barry.abcdef123456", "v0.2.19-barry.2", 1},
		{"0.2.19-barry.abcdef123456", "0.2.19-barry.fedcba654321", 0},
		{"v0.2.19", "0.2.20", -1},
		{"0.2.19-rc1", "0.2.19", 0},
	} {
		require.Equal(t, testCase.want, compareVersions(testCase.current, testCase.latest), "%s / %s", testCase.current, testCase.latest)
	}
}

func TestForkUpdateCheckFiltersUpstreamAndSeparatesCache(t *testing.T) {
	upstreamCache, err := json.Marshal(map[string]any{
		"latest": "999.0.0", "timestamp": time.Now().Unix(),
		"release_info": map[string]any{"html_url": "https://github.com/Wei-Shaw/sub2api/releases/tag/v999.0.0"},
	})
	require.NoError(t, err)
	cache := &updateServiceCacheStub{data: string(upstreamCache)}
	github := &updateServiceGitHubClientStub{recentReleases: []*GitHubRelease{
		{TagName: "v999.0.0"},
		{TagName: "v0.2.19-barry.2"},
		{TagName: "v0.2.19-barry.10", Prerelease: true, HTMLURL: "https://github.com/Wei-Shaw/sub2api/releases", Assets: []GitHubAsset{{Name: "upstream-binary"}}},
		{TagName: "v0.2.19-barry.11", Draft: true},
		{TagName: "v0.2.19-barry.invalid"},
	}}
	svc := NewUpdateService(cache, github, "v0.2.19-barry.2", "release")
	info, err := svc.CheckUpdate(context.Background(), false)
	require.NoError(t, err)
	require.Equal(t, "v0.2.19-barry.10", info.LatestVersion)
	require.True(t, info.HasUpdate)
	require.False(t, info.CanAutoUpdate)
	require.Equal(t, "fork", info.UpdateChannel)
	require.Equal(t, forkPackageURL, info.ManualUpdateURL)
	require.Empty(t, info.ReleaseInfo.Assets)
	require.Equal(t, "https://github.com/BerryWang1996/sub2api/releases/tag/v0.2.19-barry.10", info.ReleaseInfo.HTMLURL)
	require.Empty(t, github.latestRepos)
	require.Equal(t, []string{forkGitHubRepo}, github.recentRepos)

	info, err = svc.CheckUpdate(context.Background(), false)
	require.NoError(t, err)
	require.True(t, info.Cached)
	require.Equal(t, []string{forkGitHubRepo}, github.recentRepos)
	upstream := NewUpdateService(cache, &updateServiceGitHubClientStub{release: &GitHubRelease{TagName: "v0.2.20"}}, "0.2.19", "release")
	upstreamInfo, err := upstream.CheckUpdate(context.Background(), false)
	require.NoError(t, err)
	require.False(t, upstreamInfo.Cached)
	require.Equal(t, "0.2.20", upstreamInfo.LatestVersion)
	require.True(t, upstreamInfo.CanAutoUpdate)
}

func TestForkUpdateUnavailableMetadataKeepsManualChannel(t *testing.T) {
	upstreamCache, err := json.Marshal(map[string]any{"latest": "999.0.0", "timestamp": time.Now().Unix()})
	require.NoError(t, err)
	for _, version := range []string{"v0.2.19-barry.2", "barry-20261028.1", "0.2.19-barry.abcdef123456"} {
		for _, failure := range []error{nil, errors.New("offline test")} {
			github := &updateServiceGitHubClientStub{recentErr: failure, recentReleases: []*GitHubRelease{{TagName: "v99.0.0"}}}
			svc := NewUpdateService(&updateServiceCacheStub{data: string(upstreamCache)}, github, version, "release")
			info, err := svc.CheckUpdate(context.Background(), true)
			require.NoError(t, err)
			require.False(t, info.HasUpdate)
			require.False(t, info.CanAutoUpdate)
			require.Equal(t, "fork", info.UpdateChannel)
			require.Equal(t, forkPackageURL, info.ManualUpdateURL)
			require.NotEmpty(t, info.Warning)
			require.Nil(t, info.ReleaseInfo)
			require.Empty(t, github.latestRepos)
		}
	}
}

func TestForkDisallowsBinaryUpdateAndRollbackBeforeNetwork(t *testing.T) {
	for _, version := range []string{"v0.2.19-barry.2", "barry-20261028.1", "0.2.19-barry.abcdef123456"} {
		github := &updateServiceGitHubClientStub{}
		svc := NewUpdateService(&updateServiceCacheStub{}, github, version, "release")
		require.ErrorIs(t, svc.PerformUpdate(context.Background()), ErrForkImageUpdateRequired)
		require.ErrorIs(t, svc.Rollback(), ErrForkImageUpdateRequired)
		require.ErrorIs(t, svc.RollbackToVersion(context.Background(), "v0.2.19-barry.1"), ErrForkImageUpdateRequired)
		versions, err := svc.ListRollbackVersions(context.Background())
		require.NoError(t, err)
		require.Empty(t, versions)
		require.Empty(t, github.latestRepos)
		require.Empty(t, github.recentRepos)
	}
}
