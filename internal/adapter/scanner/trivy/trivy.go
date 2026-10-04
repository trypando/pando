// Package trivy scans what an app deploys, with Trivy in a container.
//
// The first scanner adapter (R-317, design 09 §2). It reports vulnerable
// dependencies, leaked secrets and misconfiguration; static analysis of the
// app's own code is O-19 and is not in the score.
//
// **Trivy is given no container runtime socket.** The image it scans is saved
// by Pando — which has the socket already, because the runtime adapter needs
// one — and copied into the scanner's own filesystem as a file. R-112 forbids a
// socket in a build for the reason that applies at least as strongly here: a
// component whose job is to read untrusted code is the last one that should be
// able to start privileged containers on the host.
//
// It does get the network, because a vulnerability scanner with no vulnerability
// database is a component that reports every app as clean. The database is
// cached in a volume so the second scan does not download it again.
package trivy

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// Kind is the adapter's kind string.
const Kind = "trivy"

// The scanner image, pinned by digest.
//
// A tag moves, and a scanner whose behavior changes underneath an installation
// changes every score with it. The digest is what makes two scans of the same
// image comparable — and the version in the tag is the readable half, as in the
// Dockerfile.
const defaultImage = "aquasec/trivy:0.74.0@sha256:62b1e65e8869bc4b4c6aa4fa2b21595256c7c2f6018a9d9ad61caf87187c1969"

// cacheVolume holds Trivy's vulnerability database between scans. Roughly 50MB
// downloaded on first use; without it every scan pays for it again.
const cacheVolume = "pando-trivy-cache"

// Adapter scans images and source trees.
type Adapter struct {
	cli    *client.Client
	config Config
}

// Config is the adapter's configuration.
type Config struct {
	// Host is the Docker endpoint. Empty uses the environment, as the runtime
	// adapter does.
	Host string `json:"host,omitempty"`

	// Image overrides the scanner image. For an air-gapped install with a
	// mirror, and for pinning a different version deliberately.
	Image string `json:"image,omitempty"`

	// Timeout bounds one scan. A scan that never finishes is a deploy that
	// never finishes, and R-318 says a scanner that cannot answer must not
	// block one.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
}

func New() *Adapter { return &Adapter{} }

func (a *Adapter) Kind() string           { return Kind }
func (a *Adapter) Category() api.Category { return api.CategoryScanner }

func (a *Adapter) Configure(_ context.Context, raw json.RawMessage) error {
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a.config); err != nil {
			return errs.Wrap(errs.ValidInvalid, "The scanner's configuration could not be read.", err)
		}
	}
	if a.config.Image == "" {
		a.config.Image = defaultImage
	}
	if a.config.TimeoutSeconds <= 0 {
		a.config.TimeoutSeconds = 600
	}

	opts := []client.Opt{client.FromEnv}
	if a.config.Host != "" {
		opts = append(opts, client.WithHost(a.config.Host))
	}
	cli, err := client.New(opts...)
	if err != nil {
		return errs.Wrap(errs.AdapterUnavailable, "Could not reach the container runtime to scan with.", err)
	}
	a.cli = cli
	return nil
}

func (a *Adapter) HealthCheck(ctx context.Context) error {
	if a.cli == nil {
		return errs.New(errs.AdapterUnavailable, "The scanner is not configured.")
	}
	if _, err := a.cli.Ping(ctx, client.PingOptions{}); err != nil {
		return errs.Wrap(errs.AdapterUnavailable, "The scanner cannot reach the container runtime.", err)
	}
	return nil
}

// ScannerCapabilities is data, not a type assertion (R-254).
func (a *Adapter) ScannerCapabilities() api.ScannerCapabilities {
	return api.ScannerCapabilities{ScansImages: true, ScansSource: true}
}

// Scan reads an image, a source tree, or both.
//
// Both when both are given: an image carries the dependencies the app ships
// with, and the tree carries the secrets and the misconfiguration that never
// reach one. Findings are merged and deduplicated by ID and target, because a
// dependency that appears in both should cost what it costs once.
func (a *Adapter) Scan(ctx context.Context, req api.ScanRequest) (api.ScanResult, error) {
	if a.cli == nil {
		return api.ScanResult{}, errs.New(errs.AdapterUnavailable, "The scanner is not configured.")
	}
	if req.Image == "" && req.SourceDir == "" {
		return api.ScanResult{}, errs.New(errs.ValidInvalid,
			"There is nothing to scan: this app has neither a built image nor a checkout.")
	}

	ctx, cancel := context.WithTimeout(ctx, time.Duration(a.config.TimeoutSeconds)*time.Second)
	defer cancel()

	if err := a.ensureCache(ctx); err != nil {
		return api.ScanResult{}, err
	}

	var findings []api.Finding

	if req.Image != "" {
		found, err := a.scanImage(ctx, req.Image)
		if err != nil {
			return api.ScanResult{}, err
		}
		findings = append(findings, found...)
	}

	if req.SourceDir != "" {
		found, err := a.scanSource(ctx, req.SourceDir)
		if err != nil {
			return api.ScanResult{}, err
		}
		findings = append(findings, found...)
	}

	return api.ScanResult{
		Findings: dedupe(findings),
		Scanner:  a.scannerName(),
		Ran:      time.Now().UTC(),
	}, nil
}

func (a *Adapter) scannerName() string {
	// The digest, not the tag: two scans are comparable only if the thing that
	// produced them was the same thing.
	return "trivy " + a.config.Image
}

func (a *Adapter) ensureCache(ctx context.Context) error {
	_, err := a.cli.VolumeInspect(ctx, cacheVolume, client.VolumeInspectOptions{})
	if err == nil {
		return nil
	}
	if !cerrdefs.IsNotFound(err) {
		return errs.Wrap(errs.AdapterFailed, "Could not prepare the scanner's cache.", err)
	}
	if _, err := a.cli.VolumeCreate(ctx, client.VolumeCreateOptions{Name: cacheVolume}); err != nil {
		return errs.Wrap(errs.AdapterFailed, "Could not prepare the scanner's cache.", err)
	}
	return nil
}

// scanImage saves the image and hands the scanner the file.
func (a *Adapter) scanImage(ctx context.Context, ref string) ([]api.Finding, error) {
	saved, err := a.save(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = saved.Close()
		_ = os.Remove(saved.Name())
	}()

	info, err := saved.Stat()
	if err != nil {
		return nil, errs.Wrap(errs.AdapterFailed, "Could not read the image to scan it.", err)
	}

	out, err := a.run(ctx,
		[]string{"image", "--input", "/scan/image.tar", "--format", "json", "--quiet",
			"--scanners", "vuln,secret,misconfig"},
		copyIn{path: "scan/image.tar", size: info.Size(), body: saved},
		nil)
	if err != nil {
		return nil, err
	}
	return parse(out)
}

// scanSource copies the checkout in and scans the tree.
func (a *Adapter) scanSource(ctx context.Context, dir string) ([]api.Finding, error) {
	archive, err := tarDir(dir)
	if err != nil {
		return nil, err
	}

	out, err := a.run(ctx,
		[]string{"fs", "--format", "json", "--quiet", "--scanners", "vuln,secret,misconfig", "/scan/src"},
		copyIn{},
		archive)
	if err != nil {
		return nil, err
	}
	return parse(out)
}

// save streams `docker save` into a temporary file.
//
// A file rather than memory: an image is hundreds of megabytes and Pando is not
// the place to hold one, and the copy into the scanner needs the size up front
// to write a tar header.
func (a *Adapter) save(ctx context.Context, ref string) (*os.File, error) {
	rc, err := a.cli.ImageSave(ctx, []string{ref})
	if err != nil {
		return nil, errs.Wrap(errs.AdapterFailed,
			fmt.Sprintf("Could not read the image %q to scan it.", ref), err)
	}
	defer func() { _ = rc.Close() }()

	f, err := os.CreateTemp("", "pando-scan-*.tar")
	if err != nil {
		return nil, errs.Wrap(errs.AdapterFailed, "Could not read the image to scan it.", err)
	}
	if _, err := io.Copy(f, rc); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, errs.Wrap(errs.AdapterFailed, "Could not read the image to scan it.", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, errs.Wrap(errs.AdapterFailed, "Could not read the image to scan it.", err)
	}
	return f, nil
}

// copyIn is one file to place in the scanner before it starts.
type copyIn struct {
	path string
	size int64
	body io.Reader
}

// ensureImage fetches the scanner image if it is not already here.
func (a *Adapter) ensureImage(ctx context.Context) error {
	if _, err := a.cli.ImageInspect(ctx, a.config.Image); err == nil {
		return nil
	}

	rc, err := a.cli.ImagePull(ctx, a.config.Image, client.ImagePullOptions{})
	if err != nil {
		return errs.Wrap(errs.AdapterFailed,
			"Could not fetch the scanner image "+a.config.Image+".", err).
			WithRemedy("The host needs to reach the registry once, or the image can be pulled " +
				"by hand and the adapter pointed at a mirror.")
	}
	defer func() { _ = rc.Close() }()

	// Drained, because a pull that is not read to the end is a pull that does
	// not finish.
	_, _ = io.Copy(io.Discard, rc)
	return nil
}

// run starts the scanner and returns its stdout.
func (a *Adapter) run(ctx context.Context, cmd []string, file copyIn, archive io.Reader) ([]byte, error) {
	return a.runWith(ctx, nil, cmd, file, archive)
}

func (a *Adapter) runWith(ctx context.Context, entrypoint, cmd []string, file copyIn, archive io.Reader) ([]byte, error) {
	// The scanner's own image, fetched if this host does not have it yet.
	//
	// It is pinned by digest, so fetching it is not a decision about what to
	// run — the bytes are the ones this version of Pando was built against.
	// Without this the first scan on a fresh install failed with the daemon's
	// "No such image", which reads as though the scanner were misconfigured.
	if err := a.ensureImage(ctx); err != nil {
		return nil, err
	}

	created, err := a.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image:        a.config.Image,
			Entrypoint:   entrypoint,
			Cmd:          cmd,
			AttachStdout: true,
			AttachStderr: true,
		},
		HostConfig: &container.HostConfig{
			// The database cache, and nothing else of the host's. No socket, no
			// app volume, no app network.
			Mounts: []mount.Mount{{
				Type: mount.TypeVolume, Source: cacheVolume, Target: "/root/.cache",
			}},
			AutoRemove:  false,
			CapDrop:     []string{"ALL"},
			SecurityOpt: []string{"no-new-privileges"},
		},
	})
	if err != nil {
		return nil, errs.Wrap(errs.AdapterFailed, "Could not start the scanner.", err)
	}
	defer func() {
		_, _ = a.cli.ContainerRemove(context.WithoutCancel(ctx), created.ID,
			client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	}()

	if file.body != nil {
		wrapped, err := wrap(file)
		if err != nil {
			return nil, err
		}
		if _, err := a.cli.CopyToContainer(ctx, created.ID, client.CopyToContainerOptions{DestinationPath: "/", Content: wrapped}); err != nil {
			return nil, errs.Wrap(errs.AdapterFailed, "Could not give the scanner the image to scan.", err)
		}
	}
	if archive != nil {
		if _, err := a.cli.CopyToContainer(ctx, created.ID, client.CopyToContainerOptions{DestinationPath: "/", Content: archive}); err != nil {
			return nil, errs.Wrap(errs.AdapterFailed, "Could not give the scanner the source to scan.", err)
		}
	}

	attached, err := a.cli.ContainerAttach(ctx, created.ID, client.ContainerAttachOptions{
		Stream: true, Stdout: true, Stderr: true,
	})
	if err != nil {
		return nil, errs.Wrap(errs.AdapterFailed, "Could not start the scanner.", err)
	}
	defer attached.Close()

	if _, err := a.cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return nil, errs.Wrap(errs.AdapterFailed, "Could not start the scanner.", err)
	}

	var stdout, stderr bytes.Buffer
	copyDone := make(chan error, 1)
	go func() {
		// Demultiplexed: the report is on stdout and the progress is on stderr,
		// and reading them as one stream produces JSON with a download bar in
		// the middle of it.
		_, err := stdcopy.StdCopy(&stdout, &stderr, attached.Reader)
		copyDone <- err
	}()

	wait := a.cli.ContainerWait(ctx, created.ID, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})

	if err := <-copyDone; err != nil {
		return nil, errs.Wrap(errs.AdapterFailed, "Could not read the scanner's report.", err)
	}

	select {
	case err := <-wait.Error:
		return nil, errs.Wrap(errs.AdapterFailed, "The scanner did not finish.", err)
	case status := <-wait.Result:
		if status.StatusCode != 0 {
			return nil, errs.Newf(errs.AdapterFailed,
				"The scanner exited with status %d: %s", status.StatusCode, tail(stderr.String()))
		}
		return stdout.Bytes(), nil
	case <-ctx.Done():
		return nil, errs.Wrap(errs.AdapterFailed, "The scan took too long and was stopped.", ctx.Err())
	}
}

// wrap puts one file into a tar stream, which is what CopyToContainer takes.
func wrap(file copyIn) (io.Reader, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name: file.path, Mode: 0o600, Size: file.size, ModTime: time.Now(),
	}); err != nil {
		return nil, errs.Wrap(errs.AdapterFailed, "Could not give the scanner the image to scan.", err)
	}
	header := buf.Bytes()

	// The header and the trailer are small; the body is not, and it streams.
	return io.MultiReader(
		bytes.NewReader(header),
		file.body,
		padding(file.size),
		bytes.NewReader(make([]byte, 1024)), // tar's two empty blocks
	), nil
}

// padding is the zero bytes that round a tar entry up to 512.
func padding(size int64) io.Reader {
	if rem := size % 512; rem != 0 {
		return bytes.NewReader(make([]byte, 512-rem))
	}
	return bytes.NewReader(nil)
}

// tarDir archives a directory as `scan/src/...`.
func tarDir(dir string) (io.Reader, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	err := walk(dir, func(rel string, info os.FileInfo, body io.Reader) error {
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = "scan/src/" + rel

		// Ownership and mode are normalized rather than carried across. A host
		// uid means nothing inside the scanner, and a source file that happens
		// to be 0600 on somebody's laptop arrived unreadable — which a scanner
		// reports as a clean tree rather than as a file it could not open. That
		// is the worst failure this component has: a pass nobody earned.
		header.Uid, header.Gid = 0, 0
		header.Uname, header.Gname = "", ""
		if info.IsDir() {
			header.Mode = 0o755
		} else {
			header.Mode = 0o644
		}
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if body != nil {
			_, err = io.Copy(tw, body)
		}
		return err
	})
	if err != nil {
		return nil, errs.Wrap(errs.AdapterFailed, "Could not read the app's source to scan it.", err)
	}
	if err := tw.Close(); err != nil {
		return nil, errs.Wrap(errs.AdapterFailed, "Could not read the app's source to scan it.", err)
	}
	return &buf, nil
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 400 {
		return "…" + s[len(s)-400:]
	}
	return s
}

// dedupe keeps one finding per id and target.
func dedupe(in []api.Finding) []api.Finding {
	seen := map[string]bool{}
	out := make([]api.Finding, 0, len(in))
	for _, f := range in {
		key := f.ID + "\x00" + f.Target
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, f)
	}
	return out
}

// Info describes this kind of adapter for the forms that configure one
// (api.KindInfo, R-261).
func Info() api.KindInfo {
	return api.KindInfo{
		Category:    api.CategoryScanner,
		Kind:        Kind,
		Name:        "Trivy",
		Description: "Scans source and images for known vulnerabilities and gives each app a security score.",
		IDPrefix:    "scn_",
		Fields: []api.Field{
			{Key: "image", Label: "Image", Type: "string", Help: "The Trivy image to run.", Default: "aquasec/trivy:0.74.0 (pinned)", Advanced: true},
			{Key: "host", Label: "Docker host", Type: "string", Help: "Where to run it.", Default: "DOCKER_HOST, or the local socket", Advanced: true},
			{Key: "timeout_seconds", Label: "Timeout", Type: "int", Help: "Seconds a scan may take.", Default: "600", Advanced: true},
		},
	}
}
