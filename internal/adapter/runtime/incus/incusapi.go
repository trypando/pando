package incus

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/pkg/sftp"

	"github.com/trypando/pando/internal/adapter/runtime/incus/incusapi"
)

// errNotFound is what every incusAPI method returns for something that does
// not exist, so callers need not know how Incus says it.
var errNotFound = errors.New("not found")

// incusAPI is what the adapter asks of Incus, in its own terms: every call
// that starts an operation waits for it, honoring ctx. The real one is
// server, over incusapi.Client; tests use a fake.
type incusAPI interface {
	Server(ctx context.Context) (*incusapi.Server, error)

	// Members is the cluster's members, or nil on a server that is not
	// clustered.
	Members(ctx context.Context) ([]incusapi.ClusterMember, error)

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

// server is incusAPI over the REST client, in one project.
type server struct {
	c *incusapi.Client
}

func notFound(err error) error {
	if incusapi.IsStatus(err, http.StatusNotFound) {
		return errNotFound
	}
	return err
}

// forbidden reports whether Incus refused a call for want of permission.
func forbidden(err error) bool {
	return incusapi.IsStatus(err, http.StatusForbidden)
}

func esc(s string) string { return url.PathEscape(s) }

func (s *server) Server(ctx context.Context) (*incusapi.Server, error) {
	var srv incusapi.Server
	_, err := s.c.Get(ctx, "/1.0", nil, &srv)
	return &srv, err
}

func (s *server) Members(ctx context.Context) ([]incusapi.ClusterMember, error) {
	srv, err := s.Server(ctx)
	if err != nil {
		return nil, err
	}
	if !srv.Environment.ServerClustered {
		return nil, nil
	}
	var out []incusapi.ClusterMember
	_, err = s.c.Get(ctx, "/1.0/cluster/members", url.Values{"recursion": {"1"}}, &out)
	return out, err
}

func (s *server) Resources(ctx context.Context, member string) (*incusapi.Resources, error) {
	c := s.c
	if member != "" {
		c = c.UseTarget(member)
	}
	var r incusapi.Resources
	_, err := c.Get(ctx, "/1.0/resources", nil, &r)
	return &r, notFound(err)
}

func (s *server) Instances(ctx context.Context) ([]incusapi.InstanceFull, error) {
	var out []incusapi.InstanceFull
	_, err := s.c.Get(ctx, "/1.0/instances", url.Values{"recursion": {"2"}}, &out)
	return out, err
}

func (s *server) Instance(ctx context.Context, name string) (*incusapi.Instance, string, error) {
	var inst incusapi.Instance
	etag, err := s.c.Get(ctx, "/1.0/instances/"+esc(name), nil, &inst)
	if err != nil {
		return nil, "", notFound(err)
	}
	return &inst, etag, nil
}

func (s *server) InstanceState(ctx context.Context, name string) (*incusapi.InstanceState, error) {
	var st incusapi.InstanceState
	if _, err := s.c.Get(ctx, "/1.0/instances/"+esc(name)+"/state", nil, &st); err != nil {
		return nil, notFound(err)
	}
	return &st, nil
}

func (s *server) CreateInstance(ctx context.Context, target string, post incusapi.InstancesPost) error {
	c := s.c
	if target != "" {
		c = c.UseTarget(target)
	}
	return notFound(c.Send(ctx, http.MethodPost, "/1.0/instances", post, ""))
}

func (s *server) UpdateInstance(ctx context.Context, name string, put incusapi.InstancePut, etag string) error {
	return notFound(s.c.Send(ctx, http.MethodPut, "/1.0/instances/"+esc(name), put, etag))
}

func (s *server) SetState(ctx context.Context, name, action string, force bool) error {
	return notFound(s.c.Send(ctx, http.MethodPut, "/1.0/instances/"+esc(name)+"/state",
		incusapi.InstanceStatePut{Action: action, Force: force, Timeout: -1}, ""))
}

func (s *server) DeleteInstance(ctx context.Context, name string) error {
	return notFound(s.c.Send(ctx, http.MethodDelete, "/1.0/instances/"+esc(name), nil, ""))
}

func (s *server) Exec(ctx context.Context, name string, command []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	code, err := s.c.Exec(ctx, name, command, stdin, stdout, stderr)
	return code, notFound(err)
}

func (s *server) Network(ctx context.Context, name string) (*incusapi.Network, string, error) {
	var n incusapi.Network
	etag, err := s.c.Get(ctx, "/1.0/networks/"+esc(name), nil, &n)
	if err != nil {
		return nil, "", notFound(err)
	}
	return &n, etag, nil
}

func (s *server) UpdateNetwork(ctx context.Context, name string, put incusapi.NetworkPut, etag string) error {
	return notFound(s.c.Send(ctx, http.MethodPut, "/1.0/networks/"+esc(name), put, etag))
}

func (s *server) ACL(ctx context.Context, name string) (*incusapi.NetworkACL, string, error) {
	var acl incusapi.NetworkACL
	etag, err := s.c.Get(ctx, "/1.0/network-acls/"+esc(name), nil, &acl)
	if err != nil {
		return nil, "", notFound(err)
	}
	return &acl, etag, nil
}

func (s *server) CreateACL(ctx context.Context, acl incusapi.NetworkACLsPost) error {
	return s.c.Send(ctx, http.MethodPost, "/1.0/network-acls", acl, "")
}

func (s *server) UpdateACL(ctx context.Context, name string, put incusapi.NetworkACLPut, etag string) error {
	return notFound(s.c.Send(ctx, http.MethodPut, "/1.0/network-acls/"+esc(name), put, etag))
}

func (s *server) Profile(ctx context.Context, name string) (*incusapi.Profile, error) {
	var p incusapi.Profile
	if _, err := s.c.Get(ctx, "/1.0/profiles/"+esc(name), nil, &p); err != nil {
		return nil, notFound(err)
	}
	return &p, nil
}

func (s *server) PoolDriver(ctx context.Context, pool string) (string, error) {
	var p incusapi.StoragePool
	if _, err := s.c.Get(ctx, "/1.0/storage-pools/"+esc(pool), nil, &p); err != nil {
		return "", notFound(err)
	}
	return p.Driver, nil
}

func volumePath(pool, name string) string {
	return "/1.0/storage-pools/" + esc(pool) + "/volumes/custom/" + esc(name)
}

func (s *server) Volume(ctx context.Context, pool, name string) (*incusapi.StorageVolume, error) {
	var v incusapi.StorageVolume
	if _, err := s.c.Get(ctx, volumePath(pool, name), nil, &v); err != nil {
		return nil, notFound(err)
	}
	return &v, nil
}

func (s *server) Volumes(ctx context.Context, pool string) ([]incusapi.StorageVolume, error) {
	var out []incusapi.StorageVolume
	_, err := s.c.Get(ctx, "/1.0/storage-pools/"+esc(pool)+"/volumes", url.Values{"recursion": {"1"}}, &out)
	return out, err
}

func (s *server) CreateVolume(ctx context.Context, target, pool string, post incusapi.StorageVolumesPost) error {
	c := s.c
	if target != "" && !strings.HasPrefix(target, "@") {
		c = c.UseTarget(target)
	}
	return c.Send(ctx, http.MethodPost, "/1.0/storage-pools/"+esc(pool)+"/volumes/custom", post, "")
}

func (s *server) DeleteVolume(ctx context.Context, pool, name string) error {
	return notFound(s.c.Send(ctx, http.MethodDelete, volumePath(pool, name), nil, ""))
}

func (s *server) VolumeFiles(ctx context.Context, pool, name string) (*sftp.Client, error) {
	conn, err := s.c.Upgrade(ctx, volumePath(pool, name)+"/sftp", "sftp")
	if err != nil {
		return nil, notFound(err)
	}
	c, err := sftp.NewClientPipe(conn, conn, sftp.MaxPacketUnchecked(128*1024))
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	go func() {
		_ = c.Wait()
		_ = conn.Close()
	}()
	return c, nil
}

func (s *server) ImageAlias(ctx context.Context, name string) (*incusapi.ImageAliasesEntry, error) {
	var a incusapi.ImageAliasesEntry
	if _, err := s.c.Get(ctx, "/1.0/images/aliases/"+esc(name), nil, &a); err != nil {
		return nil, notFound(err)
	}
	return &a, nil
}

func (s *server) Publish(ctx context.Context, instance, alias string, properties map[string]string) error {
	return s.c.Send(ctx, http.MethodPost, "/1.0/images", incusapi.ImagesPost{
		ImagePut: incusapi.ImagePut{Properties: properties},
		Source:   &incusapi.ImagesPostSource{Type: "instance", Name: instance},
		Aliases:  []incusapi.ImageAlias{{Name: alias}},
	}, "")
}

var _ incusAPI = (*server)(nil)
