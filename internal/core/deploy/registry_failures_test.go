package deploy

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/trypando/pando/internal/core/imageregistry"
	"github.com/trypando/pando/internal/core/source"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/log"
	"github.com/trypando/pando/internal/secret"
)

// unreadable is an install registry whose settings cannot be read.
type unreadable struct{ err error }

func (u unreadable) Current(context.Context) (*imageregistry.Registry, error) { return nil, u.err }

// ecrRegistry is an install registry whose credential cannot be resolved for
// it: ECR keys for a host that is not ECR.
func ecrRegistry(t *testing.T) imageregistry.Provider {
	t.Helper()
	reg, err := imageregistry.New(imageregistry.Config{URL: "https://registry.internal:5000", Kind: "ecr",
		Username: "AKIAEXAMPLE", Password: secret.New("ecr-secret-do-not-log")})
	require.NoError(t, err)
	return imageregistry.Static(reg)
}

// TestR254_ABuildWhoseRegistryCannotBeUsedIsNotBuilt asserts that a deploy
// asked to push to the install's registry stops before building when the
// registry's settings or its credential cannot be read, and fails with the
// reason — rather than building something nothing can run, or pushing
// anonymously.
func TestR254_ABuildWhoseRegistryCannotBeUsedIsNotBuilt(t *testing.T) {
	t.Parallel()
	gone := errs.New(errs.Internal, "Pando could not read the install registry's settings.")

	b := &recordingBuilder{pushes: true}
	_, err := buildRunner(t, b, &importingRuntime{caps: pulling}).WithBuildRegistry(unreadable{gone}).
		build(context.Background(), buildApp(), &source.Checkout{}, &strings.Builder{}, nil, "app_01HQ8", "dep_1")
	require.ErrorIs(t, err, gone)
	require.Empty(t, b.asked.Strategy, "nothing was built")

	b = &recordingBuilder{pushes: true}
	_, err = buildRunner(t, b, &importingRuntime{caps: pulling}).WithBuildRegistry(ecrRegistry(t)).
		build(context.Background(), buildApp(), &source.Checkout{}, &strings.Builder{}, nil, "app_01HQ8", "dep_1")
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "not an ECR registry")
	require.Empty(t, b.asked.Strategy, "nothing was built")

	failed := errs.New(errs.BuildFailed, "The push to the registry was refused.")
	b = &recordingBuilder{pushes: true, err: failed}
	_, err = buildRunner(t, b, &importingRuntime{caps: pulling}).WithBuildRegistry(installRegistry(t, false)).
		build(context.Background(), buildApp(), &source.Checkout{}, &strings.Builder{}, nil, "app_01HQ8", "dep_1")
	require.ErrorIs(t, err, failed, "the builder's own reason")
}

// TestR194_APullWithNoUsableCredentialIsAnonymousAndSaysWhyInTheServerLog
// asserts that a built image whose registry credential cannot be had is
// pulled without one — failing with the registry's own reason — and that the
// cause is logged without the credential in it.
func TestR194_APullWithNoUsableCredentialIsAnonymousAndSaysWhyInTheServerLog(t *testing.T) {
	t.Parallel()
	const built = "registry.internal:5000/apps/app_01hq8@sha256:aaa"
	core, logs := observer.New(zap.WarnLevel)
	ctx := log.Into(context.Background(), zap.New(core))

	gone := errs.New(errs.Internal, "Pando could not read the install registry's settings.")
	require.Nil(t, (&Runner{buildRegistry: unreadable{gone}}).builtImageAuth(ctx, built))
	require.Equal(t, 1, logs.FilterMessage("could not read the install registry").Len())

	require.Nil(t, (&Runner{buildRegistry: ecrRegistry(t)}).builtImageAuth(ctx, built))
	warned := logs.FilterMessage("could not resolve the install registry's credential").All()
	require.Len(t, warned, 1)
	require.NotContains(t, warned[0].ContextMap()["error"], "ecr-secret-do-not-log")
}
