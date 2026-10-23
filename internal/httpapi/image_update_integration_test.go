//go:build integration

package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/deploy"
	"github.com/trypando/pando/internal/core/oci"
	"github.com/trypando/pando/internal/core/state"
)

// publish pushes a new random build under ref and returns its digest.
func publish(t *testing.T, ref string) string {
	t.Helper()
	img, err := random.Image(32, 1)
	require.NoError(t, err)
	parsed, err := name.ParseReference(ref, name.Insecure)
	require.NoError(t, err)
	require.NoError(t, remote.Write(parsed, img))
	digest, err := img.Digest()
	require.NoError(t, err)
	return digest.String()
}

// TestR143a_CheckingThenUpgradingDeploysTheNewBuild asserts R-143a end to end:
// a check writes nothing and reports the new build; the upgrade writes a
// revision pinning it and deploys that revision.
func TestR143a_CheckingThenUpgradingDeploysTheNewBuild(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	image := strings.TrimPrefix(srv.URL, "http://") + "/acme/web:latest"
	first := publish(t, image)

	i := newInstall(t)
	withEgressAdapters(t, i)
	i.Server.Deployer.WithImages(&oci.Images{Inspector: oci.Inspector{Insecure: true}})
	admin := i.admin()

	appID := i.createApp(admin, "web")
	body := minimalSpec()
	body["source"] = map[string]any{"type": "image", "image": image, "digest": first}
	body["workloads"].([]map[string]any)[0]["image"] = image
	i.pinSpec(admin, appID, i.writeSpec(admin, appID, body))

	var check deploy.ImageCheck
	got := i.do(admin, http.MethodGet, "/apps/"+appID+"/image-update", nil)
	require.Equal(t, http.StatusOK, got.Code, got.String())
	got.JSON(t, &check)
	require.False(t, check.Available)

	second := publish(t, image)
	got = i.do(admin, http.MethodGet, "/apps/"+appID+"/image-update", nil)
	require.Equal(t, http.StatusOK, got.Code, got.String())
	got.JSON(t, &check)
	require.True(t, check.Available)
	require.Equal(t, first, check.Current)
	require.Equal(t, second, check.Latest)
	require.Equal(t, 1, check.Revision)

	got = i.do(admin, http.MethodPost, "/apps/"+appID+"/image-update", map[string]any{"digest": check.Latest})
	require.Equal(t, http.StatusAccepted, got.Code, got.String())
	var dep state.Deployment
	got.JSON(t, &dep)
	require.Equal(t, 2, dep.SpecRevision, "the upgrade is a revision of its own")

	got = i.do(admin, http.MethodGet, "/apps/"+appID+"/specs/2", nil)
	require.Equal(t, http.StatusOK, got.Code, got.String())
	require.Contains(t, got.String(), second)
}

// An upgrade to a build the tag no longer names is refused and writes nothing.
func TestR143a_AnUpgradeToAStaleCheckIsRefused(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	image := strings.TrimPrefix(srv.URL, "http://") + "/acme/web:latest"
	first := publish(t, image)

	i := newInstall(t)
	withEgressAdapters(t, i)
	i.Server.Deployer.WithImages(&oci.Images{Inspector: oci.Inspector{Insecure: true}})
	admin := i.admin()

	appID := i.createApp(admin, "web")
	body := minimalSpec()
	body["source"] = map[string]any{"type": "image", "image": image, "digest": first}
	i.pinSpec(admin, appID, i.writeSpec(admin, appID, body))

	seen := publish(t, image)
	publish(t, image)
	got := i.do(admin, http.MethodPost, "/apps/"+appID+"/image-update", map[string]any{"digest": seen})
	require.Equal(t, http.StatusConflict, got.Code, got.String())
	require.Contains(t, got.String(), "changed since it was checked")

	got = i.do(admin, http.MethodGet, "/apps/"+appID+"/specs/2", nil)
	require.Equal(t, http.StatusNotFound, got.Code, "no revision was written")
}

// A saved edit that is not deployed blocks the upgrade, which would bury it,
// and not the check, which writes nothing.
func TestR143a_AnUndeployedEditIsNotBuriedByAnUpgrade(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	image := strings.TrimPrefix(srv.URL, "http://") + "/acme/web:latest"
	first := publish(t, image)

	i := newInstall(t)
	withEgressAdapters(t, i)
	i.Server.Deployer.WithImages(&oci.Images{Inspector: oci.Inspector{Insecure: true}})
	admin := i.admin()

	appID := i.createApp(admin, "web")
	body := minimalSpec()
	body["source"] = map[string]any{"type": "image", "image": image, "digest": first}
	i.pinSpec(admin, appID, i.writeSpec(admin, appID, body))
	body["name"] = "web, edited"
	i.writeSpec(admin, appID, body)

	second := publish(t, image)
	got := i.do(admin, http.MethodGet, "/apps/"+appID+"/image-update", nil)
	require.Equal(t, http.StatusOK, got.Code, got.String())

	got = i.do(admin, http.MethodPost, "/apps/"+appID+"/image-update", map[string]any{"digest": second})
	require.Equal(t, http.StatusConflict, got.Code, got.String())
	require.Contains(t, got.String(), "not deployed yet (revision 2)")
}

// A retried upgrade replays the deploy it started rather than writing a
// second revision (R-262).
func TestR143a_ARetriedUpgradeReplaysTheFirst(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	image := strings.TrimPrefix(srv.URL, "http://") + "/acme/web:latest"
	first := publish(t, image)

	i := newInstall(t)
	withEgressAdapters(t, i)
	i.Server.Deployer.WithImages(&oci.Images{Inspector: oci.Inspector{Insecure: true}})
	admin := i.admin()

	appID := i.createApp(admin, "web")
	body := minimalSpec()
	body["source"] = map[string]any{"type": "image", "image": image, "digest": first}
	i.pinSpec(admin, appID, i.writeSpec(admin, appID, body))

	second := publish(t, image)
	upgrade := map[string]any{"digest": second, "idempotency_key": "upgrade-1"}
	got := i.do(admin, http.MethodPost, "/apps/"+appID+"/image-update", upgrade)
	require.Equal(t, http.StatusAccepted, got.Code, got.String())
	var dep state.Deployment
	got.JSON(t, &dep)

	again := i.do(admin, http.MethodPost, "/apps/"+appID+"/image-update", upgrade)
	require.Equal(t, http.StatusAccepted, again.Code, again.String())
	require.Equal(t, "true", again.Hdr.Get("Idempotent-Replay"))
	var replayed state.Deployment
	again.JSON(t, &replayed)
	require.Equal(t, dep.ID, replayed.ID)

	got = i.do(admin, http.MethodGet, "/apps/"+appID+"/specs/3", nil)
	require.Equal(t, http.StatusNotFound, got.Code, "the retry wrote no revision")
}

// The check and the upgrade are refused where they cannot apply: an app built
// from source, someone who cannot see the app, and someone who may edit its
// spec but not deploy it (the upgrade does both).
func TestR143a_TheCheckAndUpgradeAreRefusedWhereTheyCannotApply(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	withEgressAdapters(t, i)
	i.Server.Deployer.WithImages(&oci.Images{Inspector: oci.Inspector{Insecure: true}})
	admin := i.admin()
	appID := i.createApp(admin, "web")
	body := minimalSpec()
	body["source"] = map[string]any{"type": "git", "url": "https://github.com/acme/web"}
	i.pinSpec(admin, appID, i.writeSpec(admin, appID, body))
	path := "/apps/" + appID + "/image-update"

	got := i.do(admin, http.MethodGet, path, nil)
	require.Equal(t, http.StatusConflict, got.Code, got.String())
	require.Contains(t, got.String(), "Only an app deployed from a published image")

	stranger := i.user("stranger")
	got = i.do(stranger, http.MethodGet, path, nil)
	require.GreaterOrEqual(t, got.Code, http.StatusForbidden, got.String())
	got = i.do(stranger, http.MethodPost, path, map[string]any{"digest": "sha256:b2"})
	require.GreaterOrEqual(t, got.Code, http.StatusForbidden, got.String())

	editor := i.user("editor")
	i.grantControl(appID, editor, i.customRole(admin, "spec editor", "app", "app.view", "app.spec.edit"))
	got = i.do(editor, http.MethodPost, path, map[string]any{"digest": "sha256:b2"})
	require.Equal(t, http.StatusForbidden, got.Code, got.String())
	require.Equal(t, 1, i.revisionCount(admin, appID), "a refused upgrade writes no revision")
}
