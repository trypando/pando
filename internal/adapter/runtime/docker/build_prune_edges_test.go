package docker

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/moby/moby/api/types/image"
	"github.com/stretchr/testify/require"
)

// TestR224_PruningLeavesAloneWhatItDidNotBuildOrCannotList asserts the limits
// of build-tag pruning: an image Pando did not build is never listed for
// pruning, nor one loaded with no tag, and a listing that fails removes
// nothing — a tag that stays costs disk, never a deploy.
func TestR224_PruningLeavesAloneWhatItDidNotBuildOrCannotList(t *testing.T) {
	f, a := newFakeDaemon(t, nil)
	f.on("GET /images/json", respond(http.StatusInternalServerError, map[string]string{"message": "daemon busy"}))
	f.on("DELETE /images/", respond(http.StatusOK, []map[string]string{}))
	ctx := context.Background()

	a.pruneBuildTags(ctx, "docker.io/library/nginx:1.27")
	a.pruneBuildTags(ctx, "registry.internal:5000/apps/app_1@sha256:"+fmt.Sprintf("%064d", 1))
	require.Zero(t, f.called("GET /images/json"), "not Pando's build, so not Pando's to prune")
	a.pruneBuildTags(ctx, "pando/app-1")
	require.Zero(t, f.called("GET /images/json"), "no tag, so no repository of tags to prune")

	a.pruneBuildTags(ctx, "pando/app-1:dep_01")
	require.Equal(t, 1, f.called("GET /images/json"))
	require.Zero(t, f.called("DELETE /images/"), "a listing that failed removes nothing")
}

// TestStaleBuildTagsAreChosenTheSameWayEveryTime asserts that builds made in
// the same second — one Created timestamp — are still ordered, so two passes
// agree on which tag is oldest.
func TestStaleBuildTagsAreChosenTheSameWayEveryTime(t *testing.T) {
	var images []image.Summary
	for i := 0; i < keepBuildTags+1; i++ {
		images = append(images, image.Summary{
			ID:       fmt.Sprintf("sha256:%02d", i),
			RepoTags: []string{fmt.Sprintf("pando/app-1:dep_%02d", i)},
			Created:  100,
		})
	}
	loaded := "pando/app-1:dep_99"
	first := staleBuildTags(images, "pando/app-1", loaded)
	require.Equal(t, []string{"pando/app-1:dep_01", "pando/app-1:dep_00"}, first,
		"past the kept number, the lowest-named go")

	reversed := make([]image.Summary, len(images))
	for i := range images {
		reversed[len(images)-1-i] = images[i]
	}
	require.Equal(t, first, staleBuildTags(reversed, "pando/app-1", loaded), "whatever order the daemon lists them in")
}
