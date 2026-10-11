package incus

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	incusclient "github.com/lxc/incus/v6/client"
	incusapi "github.com/lxc/incus/v6/shared/api"
	"github.com/pkg/sftp"
)

// errNotFound is what every incusAPI method returns for something that does
// not exist, so callers need not know how Incus says it.
var errNotFound = errors.New("not found")

// incusAPI is what the adapter asks of Incus, in its own terms: every call
// that starts an operation waits for it, honoring ctx. The real one wraps the
// Incus client (server below); tests use a fake.
type incusAPI interface {
	Server(ctx context.Context) (*incusapi.Server, error)

	// Members is the cluster's members, or nil on a server that is not
	// clustered.
	Members(ctx context.Context) ([]incusapi.ClusterMember, error)
	MemberState(ctx context.Context, name string) (*incusapi.ClusterMemberState, error)

	// Resources is a server's hardware: the cluster member named, or the
	// server answering when member is empty.
	Resources(ctx context.Context, member string) (*incusapi.Resources, error)

	// Instances is every instance in the project, every type, with state.
	Instances(ctx context.Context) ([]incusapi.InstanceFull, error)
	Instance(ctx context.Context, name string) (*incusapi.Instance, string, error)
	InstanceState(ctx context.Context, name string) (*incusapi.InstanceState, error)
	CreateInstance(ctx context.Context, target string, post incusapi.InstancesPost) error
	UpdateInstance(ctx context.Context, name string, put incusapi.InstancePut, etag string) error
	SetState(ctx context.Context, name, action string, force bool) error
	DeleteInstance(ctx context.Context, name string) error

	// Exec runs a command in an instance through the Incus agent and returns
	// its exit status once it ends and its output is copied.
	Exec(ctx context.Context, name string, command []string, stdin io.Reader, stdout, stderr io.Writer) (int, error)

	Network(ctx context.Context, name string) (*incusapi.Network, string, error)
	UpdateNetwork(ctx context.Context, name string, put incusapi.NetworkPut, etag string) error
	ACL(ctx context.Context, name string) (*incusapi.NetworkACL, string, error)
	CreateACL(ctx context.Context, acl incusapi.NetworkACLsPost) error
	UpdateACL(ctx context.Context, name string, put incusapi.NetworkACLPut, etag string) error
	Profile(ctx context.Context, name string) (*incusapi.Profile, error)
	PoolDriver(ctx context.Context, pool string) (string, error)

	Volume(ctx context.Context, pool, name string) (*incusapi.StorageVolume, error)
	Volumes(ctx context.Context, pool string) ([]incusapi.StorageVolume, error)
	CreateVolume(ctx context.Context, target, pool string, post incusapi.StorageVolumesPost) error
	DeleteVolume(ctx context.Context, pool, name string) error
	VolumeFiles(ctx context.Context, pool, name string) (*sftp.Client, error)

	ImageAlias(ctx context.Context, name string) (*incusapi.ImageAliasesEntry, error)
	Publish(ctx context.Context, instance, alias string, properties map[string]string) error
}

// server is incusAPI over the Incus client, in one project.
type server struct {
	c incusclient.InstanceServer
}

func newServer(c incusclient.InstanceServer, project string) *server {
	return &server{c: c.UseProject(project)}
}

func notFound(err error) error {
	if err != nil && incusapi.StatusErrorCheck(err, http.StatusNotFound) {
		return errNotFound
	}
	return err
}

// forbidden reports whether Incus refused a call for want of permission.
func forbidden(err error) bool {
	return err != nil && incusapi.StatusErrorCheck(err, http.StatusForbidden)
}

func wait(ctx context.Context, op incusclient.Operation, err error) error {
	if err != nil {
		return notFound(err)
	}
	return notFound(op.WaitContext(ctx))
}

func (s *server) Server(context.Context) (*incusapi.Server, error) {
	srv, _, err := s.c.GetServer()
	return srv, err
}

func (s *server) Members(context.Context) ([]incusapi.ClusterMember, error) {
	if !s.c.IsClustered() {
		return nil, nil
	}
	return s.c.GetClusterMembers()
}

func (s *server) MemberState(_ context.Context, name string) (*incusapi.ClusterMemberState, error) {
	st, _, err := s.c.GetClusterMemberState(name)
	return st, notFound(err)
}

func (s *server) Resources(_ context.Context, member string) (*incusapi.Resources, error) {
	if member != "" {
		return s.c.UseTarget(member).GetServerResources()
	}
	return s.c.GetServerResources()
}

func (s *server) Instances(context.Context) ([]incusapi.InstanceFull, error) {
	return s.c.GetInstancesFull(incusapi.InstanceTypeAny)
}

func (s *server) Instance(_ context.Context, name string) (*incusapi.Instance, string, error) {
	inst, etag, err := s.c.GetInstance(name)
	return inst, etag, notFound(err)
}

func (s *server) InstanceState(_ context.Context, name string) (*incusapi.InstanceState, error) {
	st, _, err := s.c.GetInstanceState(name)
	return st, notFound(err)
}

func (s *server) CreateInstance(ctx context.Context, target string, post incusapi.InstancesPost) error {
	c := s.c
	if target != "" {
		c = c.UseTarget(target)
	}
	op, err := c.CreateInstance(post)
	return wait(ctx, op, err)
}

func (s *server) UpdateInstance(ctx context.Context, name string, put incusapi.InstancePut, etag string) error {
	op, err := s.c.UpdateInstance(name, put, etag)
	return wait(ctx, op, err)
}

func (s *server) SetState(ctx context.Context, name, action string, force bool) error {
	op, err := s.c.UpdateInstanceState(name, incusapi.InstanceStatePut{Action: action, Force: force, Timeout: -1}, "")
	return wait(ctx, op, err)
}

func (s *server) DeleteInstance(ctx context.Context, name string) error {
	op, err := s.c.DeleteInstance(name)
	return wait(ctx, op, err)
}

func (s *server) Exec(ctx context.Context, name string, command []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	done := make(chan bool)
	op, err := s.c.ExecInstance(name, incusapi.InstanceExecPost{
		Command:   command,
		WaitForWS: true,
	}, &incusclient.InstanceExecArgs{Stdin: stdin, Stdout: stdout, Stderr: stderr, DataDone: done})
	if err != nil {
		return -1, notFound(err)
	}
	if err := op.WaitContext(ctx); err != nil {
		_ = op.Cancel()
		return -1, err
	}
	select {
	case <-done:
	case <-ctx.Done():
		return -1, ctx.Err()
	}
	code, ok := op.Get().Metadata["return"].(float64)
	if !ok {
		return -1, fmt.Errorf("incus did not report how %q ended", command[0])
	}
	return int(code), nil
}

func (s *server) Network(_ context.Context, name string) (*incusapi.Network, string, error) {
	n, etag, err := s.c.GetNetwork(name)
	return n, etag, notFound(err)
}

func (s *server) UpdateNetwork(_ context.Context, name string, put incusapi.NetworkPut, etag string) error {
	return notFound(s.c.UpdateNetwork(name, put, etag))
}

func (s *server) ACL(_ context.Context, name string) (*incusapi.NetworkACL, string, error) {
	acl, etag, err := s.c.GetNetworkACL(name)
	return acl, etag, notFound(err)
}

func (s *server) CreateACL(_ context.Context, acl incusapi.NetworkACLsPost) error {
	return s.c.CreateNetworkACL(acl)
}

func (s *server) UpdateACL(_ context.Context, name string, put incusapi.NetworkACLPut, etag string) error {
	return notFound(s.c.UpdateNetworkACL(name, put, etag))
}

func (s *server) Profile(_ context.Context, name string) (*incusapi.Profile, error) {
	p, _, err := s.c.GetProfile(name)
	return p, notFound(err)
}

func (s *server) PoolDriver(_ context.Context, pool string) (string, error) {
	p, _, err := s.c.GetStoragePool(pool)
	if err != nil {
		return "", notFound(err)
	}
	return p.Driver, nil
}

func (s *server) Volume(_ context.Context, pool, name string) (*incusapi.StorageVolume, error) {
	v, _, err := s.c.GetStoragePoolVolume(pool, "custom", name)
	return v, notFound(err)
}

func (s *server) Volumes(_ context.Context, pool string) ([]incusapi.StorageVolume, error) {
	return s.c.GetStoragePoolVolumes(pool)
}

func (s *server) CreateVolume(_ context.Context, target, pool string, post incusapi.StorageVolumesPost) error {
	if target != "" && !strings.HasPrefix(target, "@") {
		return s.c.UseTarget(target).CreateStoragePoolVolume(pool, post)
	}
	return s.c.CreateStoragePoolVolume(pool, post)
}

func (s *server) DeleteVolume(_ context.Context, pool, name string) error {
	return notFound(s.c.DeleteStoragePoolVolume(pool, "custom", name))
}

func (s *server) VolumeFiles(_ context.Context, pool, name string) (*sftp.Client, error) {
	c, err := s.c.GetStoragePoolVolumeFileSFTP(pool, "custom", name)
	return c, notFound(err)
}

func (s *server) ImageAlias(_ context.Context, name string) (*incusapi.ImageAliasesEntry, error) {
	a, _, err := s.c.GetImageAlias(name)
	return a, notFound(err)
}

func (s *server) Publish(ctx context.Context, instance, alias string, properties map[string]string) error {
	op, err := s.c.CreateImage(incusapi.ImagesPost{
		ImagePut: incusapi.ImagePut{Properties: properties},
		Source:   &incusapi.ImagesPostSource{Type: "instance", Name: instance},
		Aliases:  []incusapi.ImageAlias{{Name: alias}},
	}, nil)
	return wait(ctx, op, err)
}

var _ incusAPI = (*server)(nil)
