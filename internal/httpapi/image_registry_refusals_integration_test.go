//go:build integration

package httpapi_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/imageregistry"
	"github.com/trypando/pando/internal/errs"
)

// brokenRegistry is an install registry whose stored settings cannot be read.
type brokenRegistry struct{}

var errRegistryGone = errs.New(errs.Internal, "Pando could not read the install registry's settings.")

func (brokenRegistry) Describe(context.Context) (imageregistry.View, error) {
	return imageregistry.View{}, errRegistryGone
}
func (brokenRegistry) Update(context.Context, imageregistry.Change, string) (imageregistry.View, error) {
	return imageregistry.View{}, errRegistryGone
}
func (brokenRegistry) Clear(context.Context) (imageregistry.View, error) {
	return imageregistry.View{}, errRegistryGone
}

// TestR261_TheImageRegistryEndpointsSayWhenTheyCannotHelp asserts the image
// registry endpoints on an install that has no registry service, and on one
// whose settings cannot be read: the first answers with the defaults and
// refuses a change it cannot store, the second passes the failure on rather
// than answering as though no registry were configured.
func TestR261_TheImageRegistryEndpointsSayWhenTheyCannotHelp(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()

	i.Server.ImageRegistry = nil
	got := i.do(admin, http.MethodGet, "/image-registry", nil)
	require.Equal(t, http.StatusOK, got.Code, got.String())
	var view imageregistry.View
	got.JSON(t, &view)
	require.False(t, view.Configured)
	require.Equal(t, "basic", view.Kind)
	require.Equal(t, "per_app", view.Layout)

	got = i.do(admin, http.MethodPut, "/image-registry", map[string]any{"url": "https://registry.internal"})
	require.Equal(t, errs.StateInvalid, errs.Code(got.ErrorCode()), got.String())
	require.Contains(t, got.String(), "cannot store the install registry")

	got = i.do(admin, http.MethodDelete, "/image-registry", nil)
	require.Equal(t, http.StatusNoContent, got.Code, "nothing stored, so nothing to clear")

	i.Server.ImageRegistry = brokenRegistry{}
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		var body any
		if method == http.MethodPut {
			body = map[string]any{"always": true}
		}
		got := i.do(admin, method, "/image-registry", body)
		require.Equal(t, http.StatusInternalServerError, got.Code, method+" "+got.String())
		require.Equal(t, errs.Internal, errs.Code(got.ErrorCode()), method)
	}

	audit := i.do(admin, http.MethodGet, "/audit?action=install.registry.update", nil)
	require.Equal(t, http.StatusOK, audit.Code, audit.String())
	require.NotContains(t, audit.String(), "install.registry.update", "a change that failed is not audited")
}
