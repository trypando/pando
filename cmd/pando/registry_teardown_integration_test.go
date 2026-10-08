//go:build integration

package main

import (
	"context"
	"io"
	"log"
	"net/http/httptest"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/imageregistry"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/id"
)

// TestR224_ADeletedAppsBuildsAreDeletedForEveryWorkloadItEverBuilt asserts
// the teardown main wires for the install registry: the app-wide repository
// and one per workload any of the app's revisions built separately — not only
// the current revision's — are emptied, and another app's images are left
// alone. With no registry configured there is nothing to delete.
func TestR224_ADeletedAppsBuildsAreDeletedForEveryWorkloadItEverBuilt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	users := state.NewUsers(db)
	require.NoError(t, users.EnsureLocalAdapter(ctx))
	owner, err := users.Create(ctx, state.LocalAdapterID, "owner", "", "Owner", "", false)
	require.NoError(t, err)
	apps := state.NewApps(db)

	src := spec.Source{Type: spec.SourceGit, URL: "https://example.test/app"}
	app, err := apps.Create(ctx, "shop", id.New(id.App), owner.ID, owner.ID, src)
	require.NoError(t, err)
	for _, built := range []string{"api", "worker"} { // one workload per revision
		_, err := apps.CreateRevision(ctx, app.ID, &spec.AppSpec{
			SchemaVersion: spec.SchemaVersion, AppID: app.ID, Source: src,
			Workloads: []spec.Workload{
				{Name: "web", Primary: true},
				{Name: built, Build: &spec.WorkloadBuild{Dockerfile: built + "/Dockerfile"}},
				{Name: "cache", Image: "redis:7"},
			},
		}, spec.OriginManual, owner.ID)
		require.NoError(t, err)
	}

	srv := httptest.NewServer(ggcrregistry.New(ggcrregistry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	adapter := ociRegistry(t, map[string]any{"url": srv.URL, "insecure": true}, "")
	reg := imageregistry.Of("reg_1", adapter)
	push := func(appID, workload string) name.Reference {
		repo, tag := adapter.Repository(appID, workload, "dep_1")
		ref, err := name.NewTag(repo+":"+tag, name.Insecure)
		require.NoError(t, err)
		img, err := random.Image(64, 1)
		require.NoError(t, err)
		require.NoError(t, remote.Write(ref, img))
		// By digest: what a deployment records, and what a registry that
		// deleted the manifest no longer serves.
		d, err := img.Digest()
		require.NoError(t, err)
		pinned, err := name.NewDigest(repo+"@"+d.String(), name.Insecure)
		require.NoError(t, err)
		return pinned
	}
	gone := []name.Reference{push(app.ID, ""), push(app.ID, "api"), push(app.ID, "worker")}
	kept := push("app_other", "")

	require.NoError(t, registryImages(imageregistry.Static(reg), apps).DeleteApp(ctx, app.ID))
	for _, ref := range gone {
		_, err := remote.Head(ref)
		require.Error(t, err, ref.String()+" is deleted")
	}
	_, err = remote.Head(kept)
	require.NoError(t, err, "another app's image is untouched")

	require.NoError(t, registryImages(imageregistry.Static(nil), apps).DeleteApp(ctx, app.ID),
		"no registry, nothing to delete")
	require.ErrorContains(t, registryImages(failingRegistry{}, apps).DeleteApp(ctx, app.ID),
		"could not read the install registry", "retried at the next pass")
}
