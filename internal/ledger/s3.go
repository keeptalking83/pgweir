// SPDX-License-Identifier: Apache-2.0

package ledger

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3Sink exports to s3://bucket/prefix. Credentials and region come from the
// standard AWS configuration (environment, shared config, instance role).
// AWS_ENDPOINT_URL_S3 or AWS_ENDPOINT_URL selects an S3-compatible endpoint,
// which is then addressed path-style.
type S3Sink struct {
	client *s3.Client
	bucket string
	prefix string
	raw    string
}

// NewS3Sink returns a sink for s3://bucket/prefix.
func NewS3Sink(ctx context.Context, rawURL string) (Sink, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "s3" || u.Host == "" {
		return nil, fmt.Errorf("invalid S3 location %q (want s3://bucket/prefix)", rawURL)
	}
	prefix := strings.Trim(u.Path, "/")
	if prefix != "" {
		prefix += "/"
	}
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load AWS configuration: %w", err)
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	pathStyle := os.Getenv("AWS_ENDPOINT_URL_S3") != "" || os.Getenv("AWS_ENDPOINT_URL") != ""
	client := s3.NewFromConfig(cfg, func(o *s3.Options) { o.UsePathStyle = pathStyle })
	return &S3Sink{client: client, bucket: u.Host, prefix: prefix, raw: rawURL}, nil
}

func (s *S3Sink) String() string { return s.raw }

func (s *S3Sink) List(ctx context.Context, prefix string) ([]string, error) {
	p := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(s.prefix + prefix),
	})
	var names []string
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list s3://%s/%s: %w", s.bucket, s.prefix+prefix, err)
		}
		for _, o := range page.Contents {
			names = append(names, strings.TrimPrefix(aws.ToString(o.Key), s.prefix))
		}
	}
	return names, nil
}

// Put writes the object with If-None-Match: *, so S3 rejects overwrites. A
// HEAD request first also protects stores that ignore conditional writes.
func (s *S3Sink) Put(ctx context.Context, name string, data []byte) (bool, error) {
	key := s.prefix + name
	if existing, found, err := s.get(ctx, key); err != nil {
		return false, err
	} else if found {
		return false, compareExisting(key, existing, data)
	}
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		ContentType: aws.String("application/json"),
		IfNoneMatch: aws.String("*"),
	})
	if err == nil {
		return true, nil
	}
	if status(err) == http.StatusPreconditionFailed || status(err) == http.StatusConflict {
		existing, found, gerr := s.get(ctx, key)
		if gerr != nil {
			return false, gerr
		}
		if found {
			return false, compareExisting(key, existing, data)
		}
	}
	return false, fmt.Errorf("put s3://%s/%s: %w", s.bucket, key, err)
}

func (s *S3Sink) get(ctx context.Context, key string) ([]byte, bool, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if status(err) == http.StatusNotFound {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("get s3://%s/%s: %w", s.bucket, key, err)
	}
	defer func() { _ = out.Body.Close() }()
	b, err := io.ReadAll(out.Body)
	return b, true, err
}

func compareExisting(key string, existing, data []byte) error {
	if bytes.Equal(existing, data) {
		return nil
	}
	return fmt.Errorf("%s: %w", key, ErrExists)
}

func status(err error) int {
	var re *awshttp.ResponseError
	if errors.As(err, &re) {
		return re.HTTPStatusCode()
	}
	return 0
}
