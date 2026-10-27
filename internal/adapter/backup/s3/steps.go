package s3

import "github.com/trypando/pando/internal/adapter/api"

// steps walk through a bucket and a key that can use it. Provider-neutral:
// every S3-compatible store has a bucket, a region and an access key, under
// names close enough to these.
func steps() []api.Step {
	return []api.Step{
		{ID: "bucket", Label: "Bucket", Title: "Create a bucket",
			Do: []api.Action{
				{Text: "In the store's console, create a private bucket, such as `pando-backups`."},
				{Text: "Add a lifecycle rule that deletes incomplete multipart uploads after one day."},
				{Text: "Copy the bucket's region and, unless the store is Amazon S3, its S3 API endpoint."},
			},
			Fields: []string{"endpoint", "region", "bucket", "prefix", "path_style"}},
		{ID: "access_key", Label: "Access key", Title: "Create an access key",
			Do: []api.Action{
				{Text: "Create an access key limited to the bucket, allowed these actions:", Table: &api.Table{
					Columns: []string{"Action"},
					Rows: [][]string{
						{"s3:ListBucket"}, {"s3:PutObject"}, {"s3:GetObject"},
						{"s3:DeleteObject"}, {"s3:AbortMultipartUpload"},
					},
				}},
				{Text: "Copy the **Access key ID** and the **Secret access key**."},
			},
			Fields: []string{"access_key_id", "secret_access_key"}},
	}
}
