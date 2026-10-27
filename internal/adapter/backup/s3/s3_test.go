package s3_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/backup/s3"
	"github.com/trypando/pando/internal/errs"
)

const secretKey = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"

func settings(endpoint string) map[string]any {
	return map[string]any{
		"endpoint": endpoint, "region": "us-east-1", "bucket": "pando-backups",
		"prefix": "/pando/", "path_style": true,
		"credentials": map[string]string{"access_key_id": "AKIAEXAMPLE", "secret_access_key": secretKey},
	}
}

func configure(t *testing.T, cfg map[string]any, partSize int) (*s3.Adapter, error) {
	t.Helper()
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	a := s3.New()
	if partSize > 0 {
		s3.SetPartSize(a, partSize)
	}
	return a, a.Configure(context.Background(), raw)
}

func open(t *testing.T, partSize int) (*s3.Adapter, *fakeS3) {
	t.Helper()
	fake, srv := newFakeS3(t)
	a, err := configure(t, settings(srv.URL), partSize)
	require.NoError(t, err)
	return a, fake
}

func write(t *testing.T, a *s3.Adapter, name string, body []byte) error {
	t.Helper()
	w, err := a.Writer(context.Background(), name)
	require.NoError(t, err)
	if _, err := io.Copy(w, bytes.NewReader(body)); err != nil {
		_ = w.Close()
		return err
	}
	return w.Close()
}

// TestR417_AnS3BucketKeepsBackupsOffTheHost asserts the kind reports what a
// second host's join asks of the default destination (R-417, O-69), as data
// (R-254), and that Pando keeps retention.
func TestR417_AnS3BucketKeepsBackupsOffTheHost(t *testing.T) {
	a, _ := open(t, 0)
	caps := a.Capabilities()
	require.True(t, caps.OffHost)
	require.False(t, caps.OwnsRetention, "Pando prunes: the adapter cannot see a lifecycle rule (R-211)")
	require.False(t, caps.Immutable)
	require.EqualValues(t, int64(s3.PartSize)*10000, caps.MaxBundleBytes)
	require.Equal(t, s3.Kind, a.Kind())
	require.Equal(t, api.CategoryBackup, a.Category())
	require.NoError(t, s3.Info().Validate())
}

// TestR217_AnS3DestinationStoresListsReadsAndDeletesBundles asserts the whole
// interface against a bucket: a small bundle is one object, a large one is
// streamed in parts and never sent whole, List shows only bundles under the
// prefix, Reader returns the bytes written, and Delete removes one (R-217).
func TestR217_AnS3DestinationStoresListsReadsAndDeletesBundles(t *testing.T) {
	a, fake := open(t, 1024)
	ctx := context.Background()

	small := []byte("a small bundle")
	require.NoError(t, write(t, a, "bkp_small", small))
	got, ok := fake.object("pando/bkp_small")
	require.True(t, ok, "written under the prefix, with one slash")
	require.Equal(t, small, got)

	large := bytes.Repeat([]byte("0123456789abcdef"), 300) // 4800 bytes, five parts
	require.NoError(t, write(t, a, "bkp_large", large))
	got, _ = fake.object("pando/bkp_large")
	require.Equal(t, large, got)
	require.LessOrEqual(t, fake.largest, 1024, "no request carried more than one part")
	require.Zero(t, fake.pending())

	// Not bundles: a key further down, and the health check's probe.
	fake.objects["pando/nested/bkp_x"] = []byte("x")
	fake.objects["pando/.pando-write-probe"] = []byte("ok")
	fake.objects["elsewhere"] = []byte("y")
	fake.pageSize = 1 // and List follows pages

	list, err := a.List(ctx)
	require.NoError(t, err)
	names := map[string]int64{}
	for _, b := range list {
		names[b.Name] = b.SizeBytes
		require.False(t, b.StoredAt.IsZero())
	}
	require.Equal(t, map[string]int64{"bkp_small": int64(len(small)), "bkp_large": int64(len(large))}, names)

	rc, err := a.Reader(ctx, "bkp_large")
	require.NoError(t, err)
	read, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	require.Equal(t, large, read)

	require.NoError(t, a.Delete(ctx, "bkp_small"))
	_, ok = fake.object("pando/bkp_small")
	require.False(t, ok)
	require.NoError(t, a.Delete(ctx, "bkp_small"), "deleting what is gone succeeds")

	_, err = a.Reader(ctx, "bkp_small")
	require.Equal(t, errs.NotFound, errs.CodeOf(err))
}

// TestR215_AFailedS3WriteLeavesNothingThatLooksWhole asserts that a bundle is
// in the bucket only once Close succeeds: a part refused, a completion
// refused, or a write cancelled leaves no object and no pending upload.
func TestR215_AFailedS3WriteLeavesNothingThatLooksWhole(t *testing.T) {
	large := bytes.Repeat([]byte("x"), 4096)
	for _, op := range []string{"UploadPart", "CompleteMultipartUpload", "CreateMultipartUpload"} {
		t.Run(op, func(t *testing.T) {
			a, fake := open(t, 1024)
			fake.set(op, "InvalidRequest")
			err := write(t, a, "bkp_1", large)
			require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
			require.Contains(t, errs.As(err).Message, "InvalidRequest")
			_, ok := fake.object("pando/bkp_1")
			require.False(t, ok)
			require.Zero(t, fake.pending(), "the upload's parts were withdrawn")
		})
	}

	t.Run("cancelled", func(t *testing.T) {
		a, fake := open(t, 1024)
		ctx, cancel := context.WithCancel(context.Background())
		w, err := a.Writer(ctx, "bkp_1")
		require.NoError(t, err)
		_, err = w.Write(large)
		require.NoError(t, err)
		cancel()
		require.Error(t, w.Close())
		require.NoError(t, w.Close(), "closing twice is harmless")
		_, err = w.Write([]byte("more"))
		require.Error(t, err)
		_, ok := fake.object("pando/bkp_1")
		require.False(t, ok)
		require.Zero(t, fake.pending())
		require.Equal(t, 1, fake.aborted)
	})

	t.Run("a write after a failed part", func(t *testing.T) {
		a, fake := open(t, 1024)
		fake.set("UploadPart", "InvalidRequest")
		w, err := a.Writer(context.Background(), "bkp_1")
		require.NoError(t, err)
		_, err = w.Write(large)
		require.Error(t, err)
		_, err = w.Write([]byte("more"))
		require.Error(t, err, "the first failure sticks")
		require.Error(t, w.Close())
	})

	t.Run("a small bundle refused", func(t *testing.T) {
		a, fake := open(t, 1024)
		fake.set("PutObject", "InvalidRequest")
		require.Equal(t, errs.AdapterFailed, errs.CodeOf(write(t, a, "bkp_1", []byte("small"))))
	})
}

// TestR105_AnS3DestinationSaysWhatToFix asserts each refusal names the
// setting to change, and that no error carries the secret key (R-194).
func TestR105_AnS3DestinationSaysWhatToFix(t *testing.T) {
	_, srv := newFakeS3(t)
	bad := map[string]func(map[string]any){
		"endpoint":    func(c map[string]any) { c["endpoint"] = "s3.example.com" },
		"region":      func(c map[string]any) { c["region"] = "" },
		"bucket name": func(c map[string]any) { c["bucket"] = "a/b" },
		"access key":  func(c map[string]any) { delete(c, "credentials") },
	}
	for want, change := range bad {
		cfg := settings(srv.URL)
		change(cfg)
		_, err := configure(t, cfg, 0)
		require.Equal(t, errs.ValidInvalid, errs.CodeOf(err), want)
		require.Contains(t, errs.As(err).Message, want)
		require.NotEmpty(t, errs.As(err).Remedy)
	}

	a := s3.New()
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(a.Configure(context.Background(), json.RawMessage(`{`))))
	require.Equal(t, errs.Internal, errs.CodeOf(a.HealthCheck(context.Background())))
	_, err := a.List(context.Background())
	require.Equal(t, errs.Internal, errs.CodeOf(err))
	_, err = a.Writer(context.Background(), "bkp_1")
	require.Equal(t, errs.Internal, errs.CodeOf(err))

	// Amazon S3 itself: no endpoint is accepted.
	cfg := settings("")
	_, err = configure(t, cfg, 0)
	require.NoError(t, err)
}

// TestAnS3DestinationRefusesNamesOutsideItsPrefix asserts a bundle name
// cannot reach another key in the bucket.
func TestAnS3DestinationRefusesNamesOutsideItsPrefix(t *testing.T) {
	a, _ := open(t, 0)
	for _, name := range []string{"", "../x", "a/b", ".hidden", `a\b`, ".."} {
		_, err := a.Writer(context.Background(), name)
		require.Equal(t, errs.ValidInvalid, errs.CodeOf(err), name)
		_, err = a.Reader(context.Background(), name)
		require.Equal(t, errs.ValidInvalid, errs.CodeOf(err), name)
		require.Equal(t, errs.ValidInvalid, errs.CodeOf(a.Delete(context.Background(), name)), name)
	}
}

// TestAnS3HealthCheckProvesTheKeyCanWrite asserts the check writes and
// removes a probe, and fails, naming what to fix, for a refused key, a
// missing bucket, a store that cannot be reached, and a key that may write
// but not delete.
func TestAnS3HealthCheckProvesTheKeyCanWrite(t *testing.T) {
	a, fake := open(t, 0)
	require.NoError(t, a.HealthCheck(context.Background()))
	_, ok := fake.object("pando/.pando-write-probe")
	require.False(t, ok, "the probe is removed")

	fake.set("DeleteObject", "AccessDenied")
	err := a.HealthCheck(context.Background())
	require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "refused Pando's access key when asked to delete")

	t.Run("a wrong key", func(t *testing.T) {
		_, srv := newFakeS3(t)
		cfg := settings(srv.URL)
		cfg["credentials"] = map[string]string{"access_key_id": "AKIAOTHER", "secret_access_key": secretKey}
		a, err := configure(t, cfg, 0)
		require.NoError(t, err)
		err = a.HealthCheck(context.Background())
		require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
		e := errs.As(err)
		require.Contains(t, e.Message, "refused Pando's access key")
		require.Contains(t, e.Remedy, "backup destination's settings")
		require.NotContains(t, err.Error(), secretKey)
		require.NotContains(t, e.Message+e.Remedy, secretKey)
	})

	t.Run("no such bucket", func(t *testing.T) {
		_, srv := newFakeS3(t)
		cfg := settings(srv.URL)
		cfg["bucket"] = "missing"
		a, err := configure(t, cfg, 0)
		require.NoError(t, err)
		err = a.HealthCheck(context.Background())
		require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
		require.Contains(t, errs.As(err).Message, "missing does not exist")
		_, err = a.List(context.Background())
		require.Contains(t, errs.As(err).Message, "does not exist")
		_, err = a.Reader(context.Background(), "bkp_1")
		require.Contains(t, errs.As(err).Message, "does not exist")
		require.Contains(t, errs.As(a.Delete(context.Background(), "bkp_1")).Message, "does not exist")
	})

	t.Run("unreachable", func(t *testing.T) {
		_, srv := newFakeS3(t)
		url := srv.URL
		srv.Close()
		a, err := configure(t, settings(url), 0)
		require.NoError(t, err)
		err = a.HealthCheck(context.Background())
		require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))
		require.Contains(t, errs.As(err).Message, "could not reach the S3 bucket pando-backups at "+url)
	})

	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := a.HealthCheck(ctx)
		require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))
		require.Contains(t, errs.As(err).Message, "stopped waiting")
	})
}
