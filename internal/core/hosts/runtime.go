package hosts

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// The cluster's app hosts reach the runtime adapter as its configuration
// (R-256): core writes them into the docker-hosts runtime's host list, and the
// adapter alone places workloads among them. The first app host to join turns
// a single-host docker runtime into docker-hosts in place (R-418).

// Runtime kinds, by the strings their adapters register: core imports no
// adapter.
const (
	runtimeDocker      = "docker"
	runtimeDockerHosts = "docker-hosts"
)

// agentScheme starts the endpoint of a host that joined the cluster: its
// runtime is reached only through its agent (R-416). An entry with it is the
// cluster's to write; any other was written by the operator and is kept.
const agentScheme = "agent://"

// ConvertAction is the audit action of turning the runtime into docker-hosts.
const ConvertAction = "cluster.runtime.convert"

// runtimeHostName is what the docker-hosts runtime accepts as a host's name.
var runtimeHostName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)

// RuntimeHost is one entry of the docker-hosts runtime's host list, in its
// configuration's own field names.
type RuntimeHost struct {
	Name             string `json:"name"`
	Endpoint         string `json:"endpoint,omitempty"`
	CertSHA256       string `json:"cert_sha256,omitempty"`
	AgentAddress     string `json:"agent_address,omitempty"`
	Control          bool   `json:"control,omitempty"`
	TotalCPUMillis   int    `json:"total_cpu_millis,omitempty"`
	TotalMemoryBytes int64  `json:"total_memory_bytes,omitempty"`
	TotalDiskBytes   int64  `json:"total_disk_bytes,omitempty"`
}

// RuntimeHosts is the cluster's app hosts as docker-hosts entries, in the
// order they joined: each reached through its agent at the address it joined
// with, which must present the certificate it joined with. local, the
// install's own machine, is the control entry already in the list and is
// left out.
func RuntimeHosts(hosts []state.Host, local string) ([]RuntimeHost, error) {
	var out []RuntimeHost
	for _, h := range hosts {
		if h.Name == local || !slices.Contains(h.Roles, state.HostRoleApp) {
			continue
		}
		if !runtimeHostName.MatchString(h.Name) {
			return nil, errs.Newf(errs.ValidInvalid,
				"The host %q cannot run apps under this name: the Docker hosts runtime takes lower-case letters, digits and dashes, up to 40 characters.", h.Name).
				WithRemedy("Remove the host and join it again under a name such as app-host-2.")
		}
		out = append(out, RuntimeHost{
			Name: h.Name, Endpoint: agentScheme + h.Address, CertSHA256: h.CertSHA256,
		})
	}
	return out, nil
}

// CheckAppRoleJoin refuses an app-role join where the runtime cannot take
// one (R-418): Kubernetes and Incus already span machines, so a host joins
// them for the control role only.
func CheckAppRoleJoin(runtimeKind string, roles []string) error {
	if !slices.Contains(roles, state.HostRoleApp) {
		return nil
	}
	switch runtimeKind {
	case runtimeDocker, runtimeDockerHosts:
		return nil
	case "kubernetes", "incus":
		return errs.Newf(errs.ValidInvalid,
			"This install runs apps on %s, which places them across its own machines, so a host cannot join it to run apps. A host can join it for the control role only.",
			runtimeName(runtimeKind)).
			WithRemedy("Create a join token for the control role, or add the machine to the " + runtimeName(runtimeKind) + " cluster instead.")
	}
	return errs.Newf(errs.ValidInvalid,
		"This install's runtime, %s, cannot run apps on a joined host. Only Docker can. A host can join it for the control role only.", runtimeKind).
		WithRemedy("Create a join token for the control role.")
}

func runtimeName(kind string) string {
	if kind == "incus" {
		return "Incus"
	}
	return "Kubernetes"
}

// AdapterStore is the slice of state.Adapters the runtime sync uses.
type AdapterStore interface {
	List(ctx context.Context) ([]state.AdapterConfig, error)
	Save(ctx context.Context, c state.AdapterConfig) (state.Replaced, error)
}

// CredentialStore is the slice of state.AdapterCredentials it uses.
type CredentialStore interface {
	Put(ctx context.Context, adapterID, field string, v secret.Value) error
}

// RestartSignal asks every replica to restart, the way saved adapter
// configuration starts running (R-253): state.Replicas.
type RestartSignal interface {
	RequestRestart(ctx context.Context) error
}

// AuditWriter appends to the audit log.
type AuditWriter interface {
	Write(ctx context.Context, e audit.Event) error
}

// HostLister lists the cluster's hosts.
type HostLister interface {
	Hosts(ctx context.Context) ([]state.Host, error)
}

// Runtime keeps the runtime adapter's host list in line with the cluster's
// app hosts. The server calls Sync when the host list changes, and CheckJoin
// before an app-role join.
type Runtime struct {
	Hosts       HostLister
	Adapters    AdapterStore
	Credentials CredentialStore
	Restart     RestartSignal
	Audit       AuditWriter

	// Authority is the cluster's: the agents' mutual TLS is the cluster's,
	// handed to the runtime as its agent_authority credential (R-416).
	Authority *Authority

	// LocalName is the name the install's own machine takes in the host list
	// when it becomes the control host and first app host (R-418). Empty is
	// "control". It names the machine's volumes from then on, so it does not
	// change.
	LocalName string

	// LocalAgentAddress is where the control host's agent is reached, as
	// host:port: this machine's address and the agent port.
	LocalAgentAddress string

	// Declared reports whether the configuration file declares the adapter
	// with this ID (R-271), which a stored row then does not override.
	Declared func(id string) bool
}

// SyncResult is what a Sync did.
type SyncResult struct {
	AdapterID string `json:"adapter_id,omitempty"`
	Converted bool   `json:"converted"`
	Changed   bool   `json:"changed"`
}

func (r *Runtime) localName() string {
	if r.LocalName == "" {
		return "control"
	}
	return r.LocalName
}

// defaultRuntime is the stored, enabled default runtime adapter.
func (r *Runtime) defaultRuntime(ctx context.Context) (state.AdapterConfig, error) {
	all, err := r.Adapters.List(ctx)
	if err != nil {
		return state.AdapterConfig{}, err
	}
	for _, c := range all {
		if c.Category == "runtime" && c.IsDefault && c.Enabled {
			return c, nil
		}
	}
	return state.AdapterConfig{}, errs.New(errs.StateInvalid,
		"This install has no default runtime adapter stored, so Pando cannot tell where a joined host would run apps.").
		WithRemedy("Make a runtime adapter the default under System → Adapters, then join the host again.")
}

// CheckJoin refuses an app-role join the runtime cannot take, before the
// host is let in: a runtime spanning machines, or a Docker runtime that cannot
// be turned into a multi-host one.
func (r *Runtime) CheckJoin(ctx context.Context, roles []string) error {
	if !slices.Contains(roles, state.HostRoleApp) {
		return nil
	}
	rt, err := r.defaultRuntime(ctx)
	if err != nil {
		return err
	}
	if err := CheckAppRoleJoin(rt.Kind, roles); err != nil {
		return err
	}
	if err := r.checkWritable(rt); err != nil {
		return err
	}
	if rt.Kind == runtimeDocker {
		_, err = r.convertDocker(rt.Config)
	}
	return err
}

func (r *Runtime) checkWritable(rt state.AdapterConfig) error {
	if r.Declared != nil && r.Declared(rt.ID) {
		return errs.Newf(errs.StateInvalid,
			"The runtime adapter %s is set in Pando's configuration file, so Pando cannot add the cluster's hosts to it.", rt.ID).
			WithRemedy("Remove it from the configuration file so the stored adapter applies, or list the hosts in the file yourself with kind docker-hosts.")
	}
	return nil
}

// Sync writes the cluster's app hosts into the runtime adapter's host list,
// converting a single-host docker runtime first (R-418), and asks every
// replica to restart when anything changed. Running it again with the same
// hosts changes nothing.
func (r *Runtime) Sync(ctx context.Context) (SyncResult, error) {
	rt, err := r.defaultRuntime(ctx)
	if err != nil {
		return SyncResult{}, err
	}
	res := SyncResult{AdapterID: rt.ID}
	all, err := r.Hosts.Hosts(ctx)
	if err != nil {
		return res, err
	}
	app, err := RuntimeHosts(all, r.localName())
	if err != nil {
		return res, err
	}

	var cfg json.RawMessage
	switch rt.Kind {
	case runtimeDocker:
		if len(app) == 0 {
			// A cluster of one behaves as before (R-414).
			return res, nil
		}
		cfg, err = r.convertDocker(rt.Config)
		if err == nil {
			cfg, _, err = mergeHosts(cfg, app)
		}
		res.Converted = true
	case runtimeDockerHosts:
		cfg, res.Changed, err = mergeHosts(rt.Config, app)
		if err == nil && !res.Changed {
			return res, nil
		}
	default:
		if len(app) == 0 {
			return res, nil
		}
		err = CheckAppRoleJoin(rt.Kind, []string{state.HostRoleApp})
	}
	if err == nil {
		err = r.checkWritable(rt)
	}
	if err != nil {
		return res, err
	}
	res.Changed = true
	return res, r.write(ctx, rt, cfg, app, res.Converted)
}

// write saves the runtime's new configuration with the cluster authority as
// its agent authority, and signals the restart that makes it run.
func (r *Runtime) write(ctx context.Context, rt state.AdapterConfig, cfg json.RawMessage, app []RuntimeHost, converted bool) error {
	if converted {
		names := make([]string, 0, len(app)+1)
		names = append(names, r.localName())
		for _, h := range app {
			names = append(names, h.Name)
		}
		// Written before the change (R-228).
		if err := r.Audit.Write(ctx, audit.Event{
			PrincipalKind: audit.KindSystem, PrincipalID: "cluster", Action: ConvertAction,
			TargetKind: "adapter", TargetID: rt.ID,
			Detail: map[string]any{"from": runtimeDocker, "to": runtimeDockerHosts, "hosts": names, "control_host": r.localName()},
		}); err != nil {
			return err
		}
	}
	authority, err := r.Authority.PEM()
	if err != nil {
		return errs.Wrap(errs.Internal, "Pando could not read the cluster authority to hand it to the runtime.", err)
	}
	// The credential first: a configuration naming agent:// hosts with the
	// old authority would reach none of them.
	if err := r.Credentials.Put(ctx, rt.ID, "agent_authority", secret.New(string(authority))); err != nil {
		return err
	}
	// The same adapter, by its ID: apps pinned to it stay pinned, so this is
	// not a runtime swap (R-257).
	rt.Kind = runtimeDockerHosts
	rt.Config = cfg
	if _, err := r.Adapters.Save(ctx, rt); err != nil {
		return err
	}
	return r.Restart.RequestRestart(ctx)
}

// dockerSettings is what a single-host docker runtime's configuration holds
// that a docker-hosts one carries on.
type dockerSettings struct {
	Host               string `json:"host,omitempty"`
	TotalCPUMillis     int    `json:"total_cpu_millis,omitempty"`
	TotalMemoryBytes   int64  `json:"total_memory_bytes,omitempty"`
	TotalDiskBytes     int64  `json:"total_disk_bytes,omitempty"`
	PidsLimit          int64  `json:"pids_limit,omitempty"`
	NetworkPool        string `json:"network_pool,omitempty"`
	NetworkBlockBits   int    `json:"network_block_bits,omitempty"`
	OCIRuntime         string `json:"oci_runtime,omitempty"`
	EgressGatewayImage string `json:"egress_gateway_image,omitempty"`
}

// dockerHostsSettings is the docker-hosts configuration a conversion writes.
type dockerHostsSettings struct {
	Hosts            []RuntimeHost `json:"hosts"`
	NetworkPool      string        `json:"network_pool,omitempty"`
	NetworkBlockBits int           `json:"network_block_bits,omitempty"`
	OCIRuntime       string        `json:"oci_runtime,omitempty"`
	PidsLimit        int64         `json:"pids_limit,omitempty"`
	AgentImage       string        `json:"agent_image,omitempty"`
}

// convertDocker is a single-host docker configuration as a docker-hosts one
// whose one host is this machine, the control host (R-418). Its apps are
// found there by their labeled networks and volumes, as on one host, so
// nothing is re-created.
func (r *Runtime) convertDocker(raw json.RawMessage) (json.RawMessage, error) {
	var d dockerSettings
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &d); err != nil {
			return nil, errs.Wrap(errs.StateInvalid, "The Docker runtime's stored configuration could not be read, so Pando cannot add hosts to it.", err)
		}
	}
	if pool := strings.TrimSpace(d.NetworkPool); pool == "off" || pool == "docker" {
		return nil, errs.New(errs.ValidInvalid,
			`The Docker runtime's network_pool is "off", and apps on several hosts need it set: each host's agent forwards only into the app network range.`).
			WithRemedy("Set network_pool on the Docker runtime to a private range such as 10.213.0.0/16, restart Pando, then join the host again.")
	}
	if r.LocalAgentAddress == "" {
		return nil, errs.New(errs.StateInvalid,
			"Pando does not know the address other hosts reach this one at, so it cannot run apps on several hosts.").
			WithRemedy("Set server.advertise_url to an address other hosts can reach this one at, restart Pando, then join the host again.")
	}
	out := dockerHostsSettings{
		Hosts: []RuntimeHost{{
			Name: r.localName(), Control: true, Endpoint: d.Host, AgentAddress: r.LocalAgentAddress,
			TotalCPUMillis: d.TotalCPUMillis, TotalMemoryBytes: d.TotalMemoryBytes, TotalDiskBytes: d.TotalDiskBytes,
		}},
		NetworkPool:      d.NetworkPool,
		NetworkBlockBits: d.NetworkBlockBits,
		OCIRuntime:       d.OCIRuntime,
		PidsLimit:        d.PidsLimit,
		AgentImage:       d.EgressGatewayImage,
	}
	return json.Marshal(out)
}

// mergeHosts replaces the agent:// entries of a docker-hosts configuration
// with app, keeping every entry the operator wrote and every other setting as
// it was, and reports whether the host list changed.
func mergeHosts(raw json.RawMessage, app []RuntimeHost) (json.RawMessage, bool, error) {
	cfg := map[string]json.RawMessage{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, false, errs.Wrap(errs.StateInvalid, "The Docker hosts runtime's stored configuration could not be read, so Pando cannot add hosts to it.", err)
		}
	}
	current, err := hostEntries(cfg["hosts"])
	if err != nil {
		return nil, false, err
	}

	next := make([]any, 0, len(current))
	names := map[string]bool{}
	for _, e := range current {
		if endpoint, _ := e["endpoint"].(string); strings.HasPrefix(endpoint, agentScheme) {
			continue
		}
		name, _ := e["name"].(string)
		names[name] = true
		next = append(next, e)
	}
	for _, h := range app {
		if names[h.Name] {
			return nil, false, errs.Newf(errs.StateInvalid,
				"The host %s joined the cluster, and the Docker hosts runtime already lists a host of that name.", h.Name).
				WithRemedy("Rename or remove that entry in the runtime's host list, then save it.")
		}
		next = append(next, h)
	}

	before, err := json.Marshal(current)
	if err != nil {
		return nil, false, errs.Wrap(errs.Internal, "Could not encode the runtime's host list.", err)
	}
	encoded, err := json.Marshal(next)
	if err != nil {
		return nil, false, errs.Wrap(errs.Internal, "Could not encode the runtime's host list.", err)
	}
	// Compared after a round trip, so key order is not a change.
	var after []map[string]any
	_ = json.Unmarshal(encoded, &after)
	normalized, _ := json.Marshal(after)
	if string(before) == string(normalized) {
		return raw, false, nil
	}
	cfg["hosts"] = encoded
	out, err := json.Marshal(cfg)
	if err != nil {
		return nil, false, errs.Wrap(errs.Internal, "Could not encode the runtime's configuration.", err)
	}
	return out, true, nil
}

// hostEntries reads a host list: a JSON list, or a string holding one, which
// is how a form's text area saves it.
func hostEntries(raw json.RawMessage) ([]map[string]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		raw = json.RawMessage(text)
	}
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, errs.Wrap(errs.StateInvalid, "The Docker hosts runtime's stored host list is not a JSON list, so Pando cannot add hosts to it.", err).
			WithRemedy("Correct the host list under System → Adapters, then save it.")
	}
	return out, nil
}

// PEM is the authority as the runtime's agent_authority credential holds
// one: its certificate, then its key.
func (a *Authority) PEM() ([]byte, error) {
	if a == nil || a.Cert == nil || a.Key == nil {
		return nil, fmt.Errorf("no cluster authority is loaded")
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(a.Key)
	if err != nil {
		return nil, err
	}
	out := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: a.Cert.Raw})
	return append(out, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})...), nil
}
