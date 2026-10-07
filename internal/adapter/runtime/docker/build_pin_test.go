package docker

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/moby/moby/api/types/image"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
)

// TestR146_AnImportedBuildIsNamedByItsImageID asserts R-146 in the runtime:
// what ImportImage returns is the loaded image's content-addressed ID, never
// the tag it was loaded under. A deployment records it and the reconciler
// restores from it, so a later build — one refused by the security scan or the
// port check after it was loaded — cannot change what a recorded deployment
// runs.
func TestR146_AnImportedBuildIsNamedByItsImageID(t *testing.T) {
	f, a := newFakeDaemon(t, nil)
	f.on("POST /images/load", respond(http.StatusOK, map[string]string{"stream": "Loaded image: pando/app-1:dep_2\n"}))
	f.on("GET /images/pando/app-1:dep_2/json", respond(http.StatusOK, map[string]any{"Id": "sha256:second"}))
	f.on("GET /images/json", respond(http.StatusOK, []map[string]any{}))

	got, err := a.ImportImage(context.Background(), strings.NewReader("image-bytes"))
	require.NoError(t, err)
	require.Equal(t, "sha256:second", got)
	require.True(t, builtByPando(got), "an imported build's ID is accounted for by its label, not by a pulled tag")

	caps, err := a.Capabilities(context.Background())
	require.NoError(t, err)
	require.Equal(t, []api.ImageDelivery{api.ImageDeliveryImport, api.ImageDeliveryRegistry}, caps.ImageDelivery,
		"single-host Docker imports first and keeps no registry of its own (O-34)")
}

// TestR224_OldBuildTagsArePrunedAndLatestIsKept asserts that per-build tags do
// not accumulate for a live app (R-224): past the newest keepBuildTags, the
// oldest are removed, the one just loaded never is, and the legacy :latest a
// deployment from before pinning may still name is left alone.
func TestR224_OldBuildTagsArePrunedAndLatestIsKept(t *testing.T) {
	var images []image.Summary
	for i := 0; i < keepBuildTags+3; i++ {
		images = append(images, image.Summary{
			ID:       fmt.Sprintf("sha256:%02d", i),
			RepoTags: []string{fmt.Sprintf("pando/app-1:dep_%02d", i)},
			Created:  int64(i),
		})
	}
	images = append(images,
		image.Summary{ID: "sha256:old", RepoTags: []string{"pando/app-1:latest"}, Created: -1},
		image.Summary{ID: "sha256:other", RepoTags: []string{"pando/app-2:dep_00"}, Created: -2})
	loaded := fmt.Sprintf("pando/app-1:dep_%02d", keepBuildTags+2)

	stale := staleBuildTags(images, "pando/app-1", loaded)
	require.Equal(t, []string{"pando/app-1:dep_02", "pando/app-1:dep_01", "pando/app-1:dep_00"}, stale)

	require.Empty(t, staleBuildTags(images[:keepBuildTags], "pando/app-1", "pando/app-1:dep_00"),
		"no more than the kept number, nothing removed")

	f, a := newFakeDaemon(t, nil)
	f.on("GET /images/json", respond(http.StatusOK, images))
	var mu sync.Mutex
	var removed []string
	f.on("DELETE /images/*", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		removed = append(removed, strings.TrimSuffix(r.URL.Path[strings.Index(r.URL.Path, "/images/")+len("/images/"):], "/"))
		mu.Unlock()
		writeJSON(w, http.StatusOK, []map[string]string{})
	})
	a.pruneBuildTags(context.Background(), loaded)
	require.Len(t, removed, 3)
	for _, r := range removed {
		require.NotContains(t, r, "latest")
	}
	require.Equal(t, 0, f.called("DELETE /images/sha256"), "tags are removed, never images by ID or by force")
}
