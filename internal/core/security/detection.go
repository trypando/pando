package security

import (
	"context"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/audit"
)

// DetectionScanner is the service as detection uses it (R-311, R-312): scans
// with no spec ID — there is no revision yet, and there may never be one — and
// never fatal. The proposal is what detection produces, and a scanner that
// could not read a checkout or an image is recorded as a failed scan on the
// app already.
type DetectionScanner struct {
	Service *Service
	Logger  *zap.Logger
}

// detectionPrincipal is who a detection's scans are recorded as.
var detectionPrincipal = audit.Event{PrincipalKind: audit.KindSystem, PrincipalID: "detection"}

// Scans reports whether there is a scanner to scan with.
func (d DetectionScanner) Scans() bool {
	if d.Service == nil {
		return false
	}
	_, configured := d.Service.Configured()
	return configured
}

// ScanSource scans and records in one call: the checkout, and the published
// image the proposal runs by digest when it runs one, so a deploy of the same
// source and image reuses the scan.
func (d DetectionScanner) ScanSource(ctx context.Context, req api.ScanRequest) {
	if !d.Scans() {
		return
	}
	req.SpecID = ""
	if _, err := d.Service.Scan(ctx, req, detectionPrincipal); err != nil {
		d.logger().Info("could not scan an app's source during detection",
			zap.String("app_id", req.AppID), zap.Error(err))
	}
}

// ScanPart scans the source or the image detection built, recording nothing:
// the two run at once, beside the build and the trial run.
func (d DetectionScanner) ScanPart(ctx context.Context, req api.ScanRequest) (api.ScanResult, error) {
	if d.Service == nil {
		return api.ScanResult{}, noScanner()
	}
	return d.Service.ScanPart(ctx, req)
}

// RecordParts records what ScanPart read as one scan of the source and the
// image.
func (d DetectionScanner) RecordParts(ctx context.Context, req api.ScanRequest, parts []api.ScanResult, failed error) {
	if d.Service == nil {
		return
	}
	req.SpecID = ""
	if _, err := d.Service.RecordParts(ctx, req, parts, failed, detectionPrincipal); err != nil {
		d.logger().Info("could not scan an app during detection",
			zap.String("app_id", req.AppID), zap.Error(err))
	}
}

func (d DetectionScanner) logger() *zap.Logger {
	if d.Logger == nil {
		return zap.NewNop()
	}
	return d.Logger
}
