package security_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/security"
	"github.com/trypando/pando/internal/errs"
)

// TestR317_DetectionScansNothingWithoutAScanner asserts R-317 for detection:
// an installation with no scanner scans nothing, and says why when asked to.
func TestR317_DetectionScansNothingWithoutAScanner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, d := range []security.DetectionScanner{{}, {Service: &security.Service{Registry: api.NewRegistry()}}} {
		require.False(t, d.Scans())
		d.ScanSource(ctx, api.ScanRequest{AppID: "app_1"})
		_, err := d.ScanPart(ctx, api.ScanRequest{AppID: "app_1"})
		require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))
		d.RecordParts(ctx, api.ScanRequest{AppID: "app_1"}, nil, nil)
	}
	_, err := (&security.Service{Registry: api.NewRegistry()}).RecordParts(ctx, api.ScanRequest{}, nil, nil, audit.Event{})
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))
}
