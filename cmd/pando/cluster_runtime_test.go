package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/trypando/pando/internal/config"
	"github.com/trypando/pando/internal/core/hosts"
	"github.com/trypando/pando/internal/core/state"
)

type noAdapters struct{ err error }

func (n noAdapters) List(context.Context) ([]state.AdapterConfig, error) { return nil, n.err }
func (n noAdapters) Save(context.Context, state.AdapterConfig) (state.Replaced, error) {
	return state.Replaced{}, nil
}

type noHosts struct{}

func (noHosts) Hosts(context.Context) ([]state.Host, error) { return nil, nil }

func TestTheClusterRuntimeIsWiredToTheHostService(t *testing.T) {
	authority, err := hosts.LoadOrCreateAuthority(filepath.Join(t.TempDir(), "ca.pem"))
	require.NoError(t, err)
	svc := &hosts.Service{Authority: authority}
	rt := &hosts.Runtime{Hosts: noHosts{}, Adapters: noAdapters{err: errors.New("database down")}}
	cfg := &config.Config{Adapters: []config.AdapterDecl{{ID: "rt_declared"}}}
	cfg.Server.AdvertiseURL = "http://10.0.0.5:8080"
	core, logs := observer.New(zap.InfoLevel)

	wireClusterRuntime(svc, rt, cfg, zap.New(core))
	require.Same(t, rt, svc.Runtime)
	require.Same(t, authority, rt.Authority)
	require.Equal(t, "10.0.0.5:7443", rt.LocalAgentAddress)
	require.True(t, rt.Declared("rt_declared"))
	require.False(t, rt.Declared("rt_docker"))

	svc.Changed(context.Background())
	require.Equal(t, 1, logs.FilterMessageSnippet("could not be brought in line").Len())

	rt.Adapters = noAdapters{}
	svc.Changed(context.Background())
	require.Equal(t, 2, logs.FilterMessageSnippet("could not be brought in line").Len(), "no runtime stored is said, not skipped")
}

func TestTheLocalAgentAddressIsTheAdvertisedHost(t *testing.T) {
	require.Empty(t, localAgentAddress(""))
	require.Empty(t, localAgentAddress("::not a url"))
	require.Equal(t, "pando-1.internal:7443", localAgentAddress("https://pando-1.internal:8443/"))
}
