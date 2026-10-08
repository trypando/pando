package main

import (
	"go.uber.org/zap"

	adapterapi "github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/autodeploy"
	"github.com/trypando/pando/internal/core/reconciler"
	"github.com/trypando/pando/internal/core/source"
	"github.com/trypando/pando/internal/core/specgate"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/httpapi"
)

// autoDeployWiring is what auto-deploy is built from.
type autoDeployWiring struct {
	db          *state.DB
	apps        *state.Apps
	deployments *state.Deployments
	sources     source.Sources
	deployer    reconciler.Deployer
	auditor     *audit.Writer
	policy      reconciler.PolicyLoader
	authz       specgate.Authorizer
	secrets     adapterapi.SecretsAdapter
	secretsRef  string
	concurrency int
	logger      *zap.Logger
}

// wireAutoDeploy builds auto-deploy's job and the service behind its API
// (R-141, R-142). One job: the poll runs it, and a webhook asks the same job
// to check one app sooner, so the two cannot differ.
//
// The job never modifies a running app. It creates a revision and deploys it
// through the deployer — the approval service, as a person's deploy does —
// so the plan, the capacity hold, the audit event and the deploy queue are
// the same (O-32). An app whose deploys now need approval stops
// auto-deploying (R-158).
func wireAutoDeploy(w autoDeployWiring) (*reconciler.AutoDeploy, *autodeploy.Service) {
	checks := state.NewAutoDeployChecks(w.db)
	job := &reconciler.AutoDeploy{
		Apps:        w.apps,
		Deployments: w.deployments,
		Checks:      checks,
		Resolver:    refResolver{sources: w.sources},
		Deployer:    w.deployer,
		Audit:       w.auditor,
		Concurrency: w.concurrency,
		Logger:      w.logger,
		Policy:      w.policy,
	}
	service := &autodeploy.Service{
		Apps:    w.apps,
		Checks:  checks,
		Secrets: state.NewAutoDeploySecrets(w.db, w.secrets, w.secretsRef),
		Policy:  w.policy,
		Authz:   w.authz,
		Checker: job,
		Audit:   httpapi.AuditFunc(w.auditor),
		Logger:  w.logger,
	}
	return job, service
}
