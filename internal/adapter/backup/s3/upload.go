package s3

import (
	"bytes"
	"context"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/trypando/pando/internal/errs"
)

// abortTimeout bounds withdrawing a failed upload's parts.
const abortTimeout = 30 * time.Second

// upload is one bundle being written. It holds at most one part in memory.
//
// A bundle that fits in one part is a single PutObject on Close. A larger one
// becomes a multipart upload when its first part fills, sends each part as it
// fills, and completes on Close. Either way nothing is in the bucket under the
// bundle's name until Close returns without error; a failed write aborts the
// upload, so the store keeps no parts nobody will complete.
type upload struct {
	ctx context.Context
	a   *Adapter
	key string

	buf      []byte
	uploadID string
	parts    []types.CompletedPart

	err    error
	closed bool
}

func (u *upload) Write(p []byte) (int, error) {
	if u.closed {
		return 0, errs.New(errs.Internal, "The backup was written to after it was closed.")
	}
	if u.err != nil {
		return 0, u.err
	}
	written := 0
	for len(p) > 0 {
		if u.buf == nil {
			u.buf = make([]byte, 0, u.a.partSize)
		}
		n := min(u.a.partSize-len(u.buf), len(p))
		u.buf = append(u.buf, p[:n]...)
		p = p[n:]
		written += n
		if len(u.buf) == u.a.partSize {
			if err := u.sendPart(); err != nil {
				u.err = err
				return written, err
			}
		}
	}
	return written, nil
}

// sendPart uploads what is buffered as the next part, starting the multipart
// upload first if this is the first.
func (u *upload) sendPart() error {
	if u.uploadID == "" {
		out, err := u.a.client.CreateMultipartUpload(u.ctx, &awss3.CreateMultipartUploadInput{
			Bucket: aws.String(u.a.bucket), Key: aws.String(u.key),
		})
		if err != nil {
			return u.a.failure("write to", err)
		}
		u.uploadID = aws.ToString(out.UploadId)
	}
	if len(u.parts) >= maxParts {
		return errs.Newf(errs.AdapterFailed,
			"The backup is larger than an S3 upload can hold, %d parts of %d MiB.", maxParts, u.a.partSize>>20).
			WithRemedy("Back up fewer volumes at a time, or use a destination with no size limit.")
	}
	number := int32(len(u.parts) + 1) //nolint:gosec // G115: at most maxParts
	out, err := u.a.client.UploadPart(u.ctx, &awss3.UploadPartInput{
		Bucket: aws.String(u.a.bucket), Key: aws.String(u.key), UploadId: aws.String(u.uploadID),
		PartNumber: aws.Int32(number), Body: bytes.NewReader(u.buf),
	})
	if err != nil {
		return u.a.failure("write to", err)
	}
	u.parts = append(u.parts, types.CompletedPart{ETag: out.ETag, PartNumber: aws.Int32(number)})
	u.buf = u.buf[:0]
	return nil
}

// Close makes the bundle present, or leaves nothing behind.
func (u *upload) Close() error {
	if u.closed {
		return nil
	}
	u.closed = true
	if u.err == nil {
		u.err = u.ctx.Err()
	}
	if u.err == nil {
		u.err = u.finish()
	}
	if u.err != nil {
		u.abort()
	}
	u.buf = nil
	return u.err
}

func (u *upload) finish() error {
	if u.uploadID == "" {
		if _, err := u.a.client.PutObject(u.ctx, &awss3.PutObjectInput{
			Bucket: aws.String(u.a.bucket), Key: aws.String(u.key), Body: bytes.NewReader(u.buf),
		}); err != nil {
			return u.a.failure("write to", err)
		}
		return nil
	}
	if len(u.buf) > 0 {
		if err := u.sendPart(); err != nil {
			return err
		}
	}
	if _, err := u.a.client.CompleteMultipartUpload(u.ctx, &awss3.CompleteMultipartUploadInput{
		Bucket: aws.String(u.a.bucket), Key: aws.String(u.key), UploadId: aws.String(u.uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: u.parts},
	}); err != nil {
		return u.a.failure("write to", err)
	}
	return nil
}

// abort withdraws a multipart upload's parts. Best effort, on a context of its
// own so a cancelled write still cleans up; a part left behind is removed by
// the bucket's rule for incomplete uploads, which the setup steps ask for.
func (u *upload) abort() {
	if u.uploadID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(u.ctx), abortTimeout)
	defer cancel()
	_, _ = u.a.client.AbortMultipartUpload(ctx, &awss3.AbortMultipartUploadInput{
		Bucket: aws.String(u.a.bucket), Key: aws.String(u.key), UploadId: aws.String(u.uploadID),
	})
}
