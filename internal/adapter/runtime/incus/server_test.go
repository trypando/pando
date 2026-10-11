package incus

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/runtime/incus/incusapi"
)

// restIncus answers each path with canned JSON and records what was asked.
type restIncus struct {
	mu   sync.Mutex
	seen []string
}

func (r *restIncus) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.seen = append(r.seen, req.Method+" "+req.URL.Path)
	r.mu.Unlock()
	reply := func(typ string, metadata any) {
		b, _ := json.Marshal(metadata)
		env := map[string]any{"type": typ, "status_code": 200, "metadata": json.RawMessage(b)}
		if typ == "async" {
			env["status_code"] = 100
		}
		_ = json.NewEncoder(w).Encode(env)
	}
	path := req.URL.Path
	if strings.HasSuffix(path, "/sftp") {
		hj, _ := w.(http.Hijacker)
		conn, rw, _ := hj.Hijack()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: sftp\r\nConnection: Upgrade\r\n\r\n")
		_ = rw.Flush()
		srv := sftp.NewRequestServer(conn, sftp.InMemHandler())
		go func() { _ = srv.Serve(); _ = conn.Close() }()
		return
	}
	switch {
	case strings.HasPrefix(path, "/1.0/operations/"):
		reply("sync", map[string]any{"id": "op", "status": "Success"})
	case req.Method != http.MethodGet:
		reply("async", map[string]any{"id": "op", "status": "Running"})
	case path == "/1.0":
		reply("sync", incusapi.Server{Environment: incusapi.ServerEnvironment{ServerClustered: true, Project: "pando"}})
	case path == "/1.0/cluster/members":
		reply("sync", []incusapi.ClusterMember{{ServerName: "one"}})
	case path == "/1.0/resources":
		reply("sync", map[string]any{"cpu": map[string]any{"total": 8}, "memory": map[string]any{"total": 1 << 30, "used": 1 << 20}})
	case path == "/1.0/instances":
		reply("sync", []incusapi.InstanceFull{{Instance: incusapi.Instance{Name: "pando-app-1"}}})
	case path == "/1.0/instances/gone", path == "/1.0/profiles/gone", path == "/1.0/networks/gone":
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error_code": 404, "error": "not found"})
	case path == "/1.0/storage-pools/local":
		reply("sync", incusapi.StoragePool{Name: "local", Driver: "lvm"})
	case strings.HasPrefix(path, "/1.0/storage-pools/local/volumes"):
		if strings.HasSuffix(path, "/volumes") {
			reply("sync", []incusapi.StorageVolume{{Name: "pando-vol-1"}})
			return
		}
		reply("sync", incusapi.StorageVolume{Name: "pando-vol-1"})
	default:
		reply("sync", map[string]any{"name": filepath.Base(path)})
	}
}

func restServer(t *testing.T) (*restIncus, *server) {
	t.Helper()
	dir, err := os.MkdirTemp("", "incus")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "unix.socket")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	r := &restIncus{}
	srv := &http.Server{Handler: r, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	c, err := connect(Config{Address: "unix://" + sock, Project: "pando"})
	require.NoError(t, err)
	return r, &server{c: c}
}

// TestR251_TheIncusClientSpeaksIncussPaths asserts that each thing the
// adapter asks of Incus is the request Incus documents for it.
func TestR251_TheIncusClientSpeaksIncussPaths(t *testing.T) {
	r, s := restServer(t)

	srv, err := s.Server(ctx)
	require.NoError(t, err)
	require.Equal(t, "pando", srv.Environment.Project)
	members, err := s.Members(ctx)
	require.NoError(t, err)
	require.Equal(t, "one", members[0].ServerName)
	res, err := s.Resources(ctx, "one")
	require.NoError(t, err)
	require.Equal(t, uint64(8), res.CPU.Total)
	insts, err := s.Instances(ctx)
	require.NoError(t, err)
	require.Len(t, insts, 1)

	_, _, err = s.Instance(ctx, "gone")
	require.ErrorIs(t, err, errNotFound)
	_, err = s.Profile(ctx, "gone")
	require.ErrorIs(t, err, errNotFound)
	_, _, err = s.Network(ctx, "gone")
	require.ErrorIs(t, err, errNotFound)

	inst, _, err := s.Instance(ctx, "pando-app-1")
	require.NoError(t, err)
	require.Equal(t, "pando-app-1", inst.Name)
	_, err = s.InstanceState(ctx, "pando-app-1")
	require.NoError(t, err)
	require.NoError(t, s.CreateInstance(ctx, "one", incusapi.InstancesPost{Name: "pando-app-1"}))
	require.NoError(t, s.UpdateInstance(ctx, "pando-app-1", incusapi.InstancePut{}, "etag"))
	require.NoError(t, s.SetState(ctx, "pando-app-1", "start", false))
	require.NoError(t, s.DeleteInstance(ctx, "pando-app-1"))

	_, _, err = s.Network(ctx, "pando0")
	require.NoError(t, err)
	require.NoError(t, s.UpdateNetwork(ctx, "pando0", incusapi.NetworkPut{}, ""))
	_, _, err = s.ACL(ctx, "pando-ingress")
	require.NoError(t, err)
	require.NoError(t, s.CreateACL(ctx, incusapi.NetworkACLsPost{}))
	require.NoError(t, s.UpdateACL(ctx, "pando-ingress", incusapi.NetworkACLPut{}, ""))
	_, err = s.Profile(ctx, "default")
	require.NoError(t, err)
	driver, err := s.PoolDriver(ctx, "local")
	require.NoError(t, err)
	require.Equal(t, "lvm", driver)

	_, err = s.Volume(ctx, "local", "pando-vol-1")
	require.NoError(t, err)
	vols, err := s.Volumes(ctx, "local")
	require.NoError(t, err)
	require.Len(t, vols, 1)
	require.NoError(t, s.CreateVolume(ctx, "one", "local", incusapi.StorageVolumesPost{Name: "pando-vol-1"}))
	require.NoError(t, s.UpdateVolume(ctx, "local", "pando-vol-1", incusapi.StorageVolumePut{}))
	require.NoError(t, s.DeleteVolume(ctx, "local", "pando-vol-1"))
	_, err = s.ImageAlias(ctx, "pando-guest-1")
	require.NoError(t, err)
	require.NoError(t, s.Publish(ctx, "builder", "pando-guest-1", nil))
	files, err := s.VolumeFiles(ctx, "local", "pando-vol-1")
	require.NoError(t, err)
	_, err = files.Getwd()
	require.NoError(t, err)
	require.NoError(t, files.Close())

	r.mu.Lock()
	defer r.mu.Unlock()
	for _, want := range []string{
		"GET /1.0/cluster/members",
		"GET /1.0/instances/pando-app-1/state",
		"POST /1.0/instances",
		"PUT /1.0/instances/pando-app-1",
		"PUT /1.0/instances/pando-app-1/state",
		"DELETE /1.0/instances/pando-app-1",
		"PUT /1.0/networks/pando0",
		"POST /1.0/network-acls",
		"PUT /1.0/network-acls/pando-ingress",
		"POST /1.0/storage-pools/local/volumes/custom",
		"PUT /1.0/storage-pools/local/volumes/custom/pando-vol-1",
		"DELETE /1.0/storage-pools/local/volumes/custom/pando-vol-1",
		"GET /1.0/images/aliases/pando-guest-1",
		"POST /1.0/images",
	} {
		require.Contains(t, r.seen, want)
	}
}

// TestR190_AnHTTPSAddressNeedsUsableCertificates asserts that connecting
// over HTTPS takes the pinned server certificate and the client credential.
func TestR190_AnHTTPSAddressNeedsUsableCertificates(t *testing.T) {
	_, err := connect(Config{Address: "https://incus:8443", ServerCertificate: "nope", Credentials: Credentials{TLSClient: "nope"}})
	require.Error(t, err)
	_, err = clientCertificate("")
	require.ErrorContains(t, err, "certificate and its private key")
}
