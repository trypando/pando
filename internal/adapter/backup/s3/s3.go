// Package s3 stores backup bundles in an S3-compatible bucket: Amazon S3,
// MinIO, Cloudflare R2, Backblaze B2 and anything else that speaks the S3 API.
//
// The off-host destination (R-217, R-417; resolves O-69). A bundle in a bucket
// survives the host Pando runs on, so this kind reports OffHost and a second
// host may join an install whose default backup destination it is.
//
// Bundles are streamed: a write is a multipart upload sent a part at a time, so
// at most one part is held in memory whatever the bundle's size, and nothing is
// visible in the bucket until the upload completes — S3 shows a multipart
// object only once CompleteMultipartUpload returns, which is the atomic write
// the interface asks for (R-215). A read is the response body, unbuffered.
//
// Retention is Pando's (R-211): the adapter cannot see a lifecycle rule, and
// saying the bucket owns retention when it does not would keep every bundle
// forever.
package s3

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// Kind is the adapter kind.
const Kind = "s3"

const (
	// PartSize is how much of a bundle is held in memory and sent as one part
	// [P]. S3 refuses parts under 5 MiB except the last, and an upload has at
	// most maxParts of them, so this also sets the largest bundle.
	PartSize = 32 << 20

	// maxParts is S3's limit on the parts of one multipart upload.
	maxParts = 10000

	// probeName is the object HealthCheck writes and removes. List skips it,
	// as it skips every name starting with a dot.
	probeName = ".pando-write-probe"
)

// Adapter is one bucket, under an optional prefix.
type Adapter struct {
	client   *awss3.Client
	bucket   string
	prefix   string
	endpoint string
	partSize int
}

// New returns an unconfigured adapter.
func New() *Adapter { return &Adapter{partSize: PartSize} }

type config struct {
	Endpoint  string `json:"endpoint"`
	Region    string `json:"region"`
	Bucket    string `json:"bucket"`
	Prefix    string `json:"prefix"`
	PathStyle bool   `json:"path_style"`

	// Credentials arrive from encrypted storage (R-190), never from config.
	Credentials map[string]secret.Value `json:"credentials"`
}

func (a *Adapter) Kind() string           { return Kind }
func (a *Adapter) Category() api.Category { return api.CategoryBackup }

// Configure reads the bucket's settings and the access key. Nothing is sent to
// the store here; HealthCheck is what proves the key can write.
func (a *Adapter) Configure(_ context.Context, raw json.RawMessage) error {
	var cfg config
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return errs.Wrap(errs.ValidInvalid, "The S3 backup destination's settings could not be read.", err)
		}
	}
	cfg.Endpoint = strings.TrimSuffix(strings.TrimSpace(cfg.Endpoint), "/")
	if err := validate(cfg); err != nil {
		return err
	}

	keyID, secretKey := cfg.Credentials["access_key_id"], cfg.Credentials["secret_access_key"]
	opts := awss3.Options{
		Region: cfg.Region,
		Credentials: credentials.NewStaticCredentialsProvider(
			keyID.Reveal(), secretKey.Reveal(), ""),
		UsePathStyle: cfg.PathStyle,

		// Checksums only where S3 requires them. Several S3-compatible stores
		// refuse the CRC32 trailers the SDK otherwise adds to every upload,
		// and the bundle carries its own authenticated encryption (R-213).
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	}
	if cfg.Endpoint != "" {
		opts.BaseEndpoint = aws.String(cfg.Endpoint)
	}
	if a.partSize <= 0 {
		a.partSize = PartSize
	}
	a.client = awss3.New(opts)
	a.bucket = cfg.Bucket
	a.prefix = normalPrefix(cfg.Prefix)
	a.endpoint = cfg.Endpoint
	return nil
}

// validate refuses settings that could never reach a bucket.
func validate(cfg config) error {
	if cfg.Endpoint != "" {
		u, err := url.Parse(cfg.Endpoint)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return errs.Newf(errs.ValidInvalid, "The S3 endpoint %q is not a URL.", cfg.Endpoint).
				WithRemedy("Set the endpoint to the store's S3 API address, such as https://s3.us-west-002.backblazeb2.com, or leave it empty for Amazon S3.")
		}
	}
	if cfg.Region == "" {
		return errs.New(errs.ValidInvalid, "The S3 backup destination needs a region.").
			WithRemedy("Set the bucket's region, such as us-east-1. Cloudflare R2 uses auto.")
	}
	if cfg.Bucket == "" || strings.Contains(cfg.Bucket, "/") {
		return errs.New(errs.ValidInvalid, "The S3 backup destination needs a bucket name, without slashes.").
			WithRemedy("Set the bucket to the name of an existing bucket, such as pando-backups.")
	}
	if cfg.Credentials["access_key_id"].IsZero() || cfg.Credentials["secret_access_key"].IsZero() {
		return errs.New(errs.ValidInvalid, "The S3 backup destination needs an access key ID and its secret access key.").
			WithRemedy("Set both. The key must be allowed to list the bucket and to put, get and delete objects in it.")
	}
	return nil
}

// normalPrefix is the prefix with exactly one trailing slash, or empty.
func normalPrefix(p string) string {
	p = strings.Trim(strings.TrimSpace(p), "/")
	if p == "" {
		return ""
	}
	return p + "/"
}

// HealthCheck writes and removes a probe object.
//
// Not a HeadBucket: a key that may read the bucket and not write to it is the
// failure this needs to catch, and it must be caught before the disaster
// rather than during it.
func (a *Adapter) HealthCheck(ctx context.Context) error {
	if a.client == nil {
		return errs.New(errs.Internal, "The S3 backup destination is not configured.")
	}
	key := a.prefix + probeName
	if _, err := a.client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket: aws.String(a.bucket), Key: aws.String(key), Body: bytes.NewReader([]byte("ok")),
	}); err != nil {
		return a.failure("write to", err)
	}
	if _, err := a.client.DeleteObject(ctx, &awss3.DeleteObjectInput{
		Bucket: aws.String(a.bucket), Key: aws.String(key),
	}); err != nil {
		return a.failure("delete from", err)
	}
	return nil
}

// Capabilities: off the host, retention Pando's, and as large as maxParts
// parts of PartSize.
func (a *Adapter) Capabilities() api.BackupCapabilities {
	return api.BackupCapabilities{
		OwnsRetention:  false,
		Immutable:      false,
		MaxBundleBytes: int64(a.partSize) * maxParts,
		OffHost:        true,
	}
}

// Writer opens a bundle as an upload that becomes visible only when Close
// returns without error.
func (a *Adapter) Writer(ctx context.Context, name string) (io.WriteCloser, error) {
	key, err := a.key(name)
	if err != nil {
		return nil, err
	}
	return &upload{ctx: ctx, a: a, key: key}, nil
}

func (a *Adapter) Reader(ctx context.Context, name string) (io.ReadCloser, error) {
	key, err := a.key(name)
	if err != nil {
		return nil, err
	}
	out, err := a.client.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String(a.bucket), Key: aws.String(key)})
	if err != nil {
		var missing *types.NoSuchKey
		if errors.As(err, &missing) || apiCode(err) == "NoSuchKey" {
			return nil, errs.New(errs.NotFound, "That backup is not in this destination.").
				WithRemedy("Check which destination it was written to, and whether a lifecycle rule on the bucket removed it.")
		}
		return nil, a.failure("read from", err)
	}
	return out.Body, nil
}

// List returns every bundle directly under the prefix. Keys further down and
// names starting with a dot are not Pando's bundles.
func (a *Adapter) List(ctx context.Context) ([]api.StoredBundle, error) {
	if a.client == nil {
		return nil, errs.New(errs.Internal, "The S3 backup destination is not configured.")
	}
	var out []api.StoredBundle
	p := awss3.NewListObjectsV2Paginator(a.client, &awss3.ListObjectsV2Input{
		Bucket: aws.String(a.bucket), Prefix: aws.String(a.prefix),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, a.failure("list", err)
		}
		for _, obj := range page.Contents {
			name := strings.TrimPrefix(aws.ToString(obj.Key), a.prefix)
			if name == "" || strings.Contains(name, "/") || strings.HasPrefix(name, ".") {
				continue
			}
			out = append(out, api.StoredBundle{
				Name: name, SizeBytes: aws.ToInt64(obj.Size), StoredAt: aws.ToTime(obj.LastModified).UTC(),
			})
		}
	}
	return out, nil
}

// Delete removes a bundle. Deleting one that is not there succeeds, as S3 does.
func (a *Adapter) Delete(ctx context.Context, name string) error {
	key, err := a.key(name)
	if err != nil {
		return err
	}
	if _, err := a.client.DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: aws.String(a.bucket), Key: aws.String(key)}); err != nil {
		return a.failure("delete from", err)
	}
	return nil
}

// key is the object key for a bundle name, refusing a name that would leave
// the prefix or nest under it. Bundle names are Pando's own IDs; this is here
// for the same reason as the local destination's path check.
func (a *Adapter) key(name string) (string, error) {
	if a.client == nil {
		return "", errs.New(errs.Internal, "The S3 backup destination is not configured.")
	}
	if name == "" || name != path.Base(name) || strings.HasPrefix(name, ".") || strings.Contains(name, `\`) {
		return "", errs.New(errs.ValidInvalid, "That is not a valid backup name.")
	}
	return a.prefix + name, nil
}

// failure turns an S3 error into what an operator can act on. The access key
// is never in it: the SDK's errors carry the key ID at most, and this names
// neither (R-194).
func (a *Adapter) failure(action string, err error) error {
	where := fmt.Sprintf("the S3 bucket %s", a.bucket)
	if a.endpoint != "" {
		where += " at " + a.endpoint
	}
	switch apiCode(err) {
	case "":
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return errs.Wrap(errs.AdapterUnavailable, fmt.Sprintf("Pando stopped waiting to %s %s.", action, where), err)
		}
		return errs.Wrap(errs.AdapterUnavailable, fmt.Sprintf("Pando could not reach %s to %s it.", where, verb(action)), err).
			WithRemedy("Check the endpoint and region in the backup destination's settings, and that this host can reach the store.")
	case "NoSuchBucket":
		return errs.Newf(errs.AdapterFailed, "The S3 bucket %s does not exist.", a.bucket).
			WithRemedy("Create the bucket, or set the backup destination's bucket to one that exists.")
	case "AccessDenied", "InvalidAccessKeyId", "SignatureDoesNotMatch", "Forbidden", "InvalidToken":
		return errs.Newf(errs.AdapterFailed, "%s refused Pando's access key when asked to %s it.", capitalize(where), verb(action)).
			WithRemedy("Check the access key ID and secret access key, and that the key may list the bucket and put, get and delete objects in it. Replace them in the backup destination's settings.")
	default:
		return errs.Wrap(errs.AdapterFailed, fmt.Sprintf("Pando could not %s %s: %s.", action, where, apiCode(err)), err)
	}
}

// verb is the action without its preposition, for "to write it".
func verb(action string) string {
	v, _, _ := strings.Cut(action, " ")
	return v
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// apiCode is the S3 error code, or "" when the error did not come from S3.
func apiCode(err error) string {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		return ae.ErrorCode()
	}
	return ""
}

var _ api.BackupAdapter = (*Adapter)(nil)

// Info describes this kind of adapter for the forms that configure one
// (api.KindInfo, R-261).
func Info() api.KindInfo {
	return api.KindInfo{
		Category:    api.CategoryBackup,
		Kind:        Kind,
		Name:        "S3-compatible bucket",
		Description: "Writes backups to a bucket in Amazon S3, Cloudflare R2, Backblaze B2, MinIO or another S3-compatible store.",
		IDPrefix:    "bkp_",
		Fields: []api.Field{
			{Key: "endpoint", Label: "Endpoint", Type: "string",
				Help: "Empty is Amazon S3.", Placeholder: "https://s3.us-west-002.backblazeb2.com"},
			{Key: "region", Label: "Region", Type: "string", Required: true,
				Help: "Cloudflare R2 uses auto.", Placeholder: "us-east-1"},
			{Key: "bucket", Label: "Bucket", Type: "string", Required: true,
				Help: "It has to exist already.", Placeholder: "pando-backups"},
			{Key: "prefix", Label: "Prefix", Type: "string",
				Help: "Empty is the top of the bucket.", Placeholder: "pando/"},
			{Key: "path_style", Label: "Path-style addresses", Type: "bool",
				Help: "Needed by MinIO and most self-hosted stores."},
			{Key: "access_key_id", Label: "Access key ID", Type: "string", Credential: true, Required: true,
				Help: "A key that can list the bucket and put, get and delete objects in it."},
			{Key: "secret_access_key", Label: "Secret access key", Type: "string", Credential: true, Required: true,
				Help: "Stored encrypted, and never shown again."},
		},
		Steps: steps(),
	}
}
