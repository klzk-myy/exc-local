package objectstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// Config selects the object-store deployment mode.
type Config struct {
	Bucket string
	Region string // default "us-east-1"

	// Endpoint, when non-empty, is a base-endpoint override
	// (http://host:port) — the devs3 stub, MinIO, R2, etc. Path-style
	// addressing is forced in this mode.
	Endpoint string

	// Static credentials; required for Endpoint mode, optional otherwise
	// (AWS mode falls back to the default chain / env).
	AccessKeyID     string
	SecretAccessKey string
}

// ConfigFromEnv builds a Config from the operator environment:
//
//	EXC_S3_ENDPOINT       base endpoint override (devs3/MinIO/R2)
//	EXC_S3_REGION         region (default us-east-1)
//	EXC_S3_ACCESS_KEY_ID / EXC_S3_SECRET_ACCESS_KEY  static creds (endpoint mode)
//
// The bucket is supplied by the caller (WAL archive vs partition WORM
// bucket differ); pass "" for env-driven local development and set it on
// the returned value.
func ConfigFromEnv(bucket string, getenv func(string) string) Config {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	cfg := Config{
		Bucket:          bucket,
		Region:          getenv("EXC_S3_REGION"),
		Endpoint:        getenv("EXC_S3_ENDPOINT"),
		AccessKeyID:     getenv("EXC_S3_ACCESS_KEY_ID"),
		SecretAccessKey: getenv("EXC_S3_SECRET_ACCESS_KEY"),
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	return cfg
}

// S3Client implements Client over aws-sdk-go-v2. One type serves both
// real AWS and endpoint-override deployments; the constructors differ.
type S3Client struct {
	api    *s3.Client
	bucket string
}

func buildAWSConfig(ctx context.Context, cfg Config) (aws.Config, error) {
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	opts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.Region),
		// Keep the wire behaviour classic: no request checksum trailer
		// negotiation, so ETag stays md5-of-body on every backend.
		awsconfig.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired),
		awsconfig.WithResponseChecksumValidation(aws.ResponseChecksumValidationWhenRequired),
	}
	if cfg.AccessKeyID != "" || cfg.SecretAccessKey != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, "")))
	}
	return awsconfig.LoadDefaultConfig(ctx, opts...)
}

// NewAWS builds a client for real S3/R2. Credentials come from the
// default chain unless Config carries a static pair. Endpoint may still be
// set for R2/MinIO deployments — path style is then forced.
func NewAWS(ctx context.Context, cfg Config) (*S3Client, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("objectstore: bucket is required")
	}
	ac, err := buildAWSConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("objectstore: aws config: %w", err)
	}
	return &S3Client{api: s3.NewFromConfig(ac, endpointOpts(cfg)), bucket: cfg.Bucket}, nil
}

// NewDev builds a client pinned to a base-endpoint override (the devs3
// stub). Path-style addressing is mandatory and credentials default to a
// throwaway pair — devs3 performs no authentication.
func NewDev(ctx context.Context, cfg Config) (*S3Client, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("objectstore: dev client requires Endpoint (e.g. http://127.0.0.1:4599)")
	}
	if cfg.AccessKeyID == "" {
		cfg.AccessKeyID = "devs3"
	}
	if cfg.SecretAccessKey == "" {
		cfg.SecretAccessKey = "devs3"
	}
	return NewAWS(ctx, cfg)
}

func endpointOpts(cfg Config) func(*s3.Options) {
	return func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
			o.UsePathStyle = true
		}
	}
}

func trimETag(s string) string { return strings.Trim(s, `"`) }

func (c *S3Client) Bucket() string { return c.bucket }

func (c *S3Client) Put(ctx context.Context, in PutInput) (Object, error) {
	if in.Key == "" {
		return Object{}, errors.New("objectstore: put requires key")
	}
	body := in.Body
	if body == nil {
		body = bytes.NewReader(nil)
	}
	// Buffer when size is unknown so Content-Length is always set — the
	// dev stub and real S3 both want a definite length for non-chunked PUTs.
	if in.Size < 0 {
		buf, err := io.ReadAll(body)
		if err != nil {
			return Object{}, fmt.Errorf("objectstore: buffer put body: %w", err)
		}
		body = bytes.NewReader(buf)
		in.Size = int64(len(buf))
	}
	pi := &s3.PutObjectInput{
		Bucket:        aws.String(c.bucket),
		Key:           aws.String(in.Key),
		Body:          body,
		ContentLength: aws.Int64(in.Size),
	}
	if in.ContentType != "" {
		pi.ContentType = aws.String(in.ContentType)
	}
	if len(in.Metadata) > 0 {
		pi.Metadata = in.Metadata
	}
	if in.ObjectLockMode != "" {
		pi.ObjectLockMode = s3types.ObjectLockMode(in.ObjectLockMode)
		if !in.ObjectLockRetainUntil.IsZero() {
			pi.ObjectLockRetainUntilDate = aws.Time(in.ObjectLockRetainUntil)
		}
	}
	if in.ServerSideEncryption != "" {
		pi.ServerSideEncryption = s3types.ServerSideEncryption(in.ServerSideEncryption)
		if in.SSEKMSKeyID != "" {
			pi.SSEKMSKeyId = aws.String(in.SSEKMSKeyID)
		}
	}
	out, err := c.api.PutObject(ctx, pi)
	if err != nil {
		return Object{}, fmt.Errorf("objectstore: put %q: %w", in.Key, err)
	}
	return Object{
		Key:  in.Key,
		Size: in.Size,
		ETag: trimETag(aws.ToString(out.ETag)),
	}, nil
}

func isNotFoundErr(err error) bool {
	var nf *s3types.NoSuchKey
	var nfound *s3types.NotFound
	var re *smithyhttp.ResponseError
	switch {
	case errors.As(err, &nf), errors.As(err, &nfound):
		return true
	case errors.As(err, &re) && re.HTTPResponse().StatusCode == http.StatusNotFound:
		return true
	}
	return false
}

func (c *S3Client) Get(ctx context.Context, key string) ([]byte, Object, error) {
	out, err := c.api.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFoundErr(err) {
			return nil, Object{}, &NotFoundError{Key: key, Err: err}
		}
		return nil, Object{}, fmt.Errorf("objectstore: get %q: %w", key, err)
	}
	defer out.Body.Close()
	data, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, Object{}, fmt.Errorf("objectstore: read %q: %w", key, err)
	}
	obj := Object{
		Key:      key,
		ETag:     trimETag(aws.ToString(out.ETag)),
		Metadata: out.Metadata,
	}
	if out.ContentLength != nil {
		obj.Size = *out.ContentLength
	}
	if out.LastModified != nil {
		obj.LastModified = *out.LastModified
	}
	if out.ServerSideEncryption != "" {
		obj.ServerSideEncryption = string(out.ServerSideEncryption)
	}
	if out.SSEKMSKeyId != nil {
		obj.SSEKMSKeyID = *out.SSEKMSKeyId
	}
	return data, obj, nil
}

func (c *S3Client) Head(ctx context.Context, key string) (Object, error) {
	out, err := c.api.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFoundErr(err) {
			return Object{}, &NotFoundError{Key: key, Err: err}
		}
		return Object{}, fmt.Errorf("objectstore: head %q: %w", key, err)
	}
	obj := Object{
		Key:      key,
		ETag:     trimETag(aws.ToString(out.ETag)),
		Metadata: out.Metadata,
	}
	if out.ContentLength != nil {
		obj.Size = *out.ContentLength
	}
	if out.LastModified != nil {
		obj.LastModified = *out.LastModified
	}
	if out.ObjectLockMode != "" {
		obj.ObjectLockMode = string(out.ObjectLockMode)
	}
	if out.ObjectLockRetainUntilDate != nil {
		obj.ObjectLockRetainUntil = *out.ObjectLockRetainUntilDate
	}
	if out.ServerSideEncryption != "" {
		obj.ServerSideEncryption = string(out.ServerSideEncryption)
	}
	if out.SSEKMSKeyId != nil {
		obj.SSEKMSKeyID = *out.SSEKMSKeyId
	}
	return obj, nil
}

func (c *S3Client) List(ctx context.Context, in ListInput) (ListOutput, error) {
	li := &s3.ListObjectsV2Input{
		Bucket: aws.String(c.bucket),
		Prefix: aws.String(in.Prefix),
	}
	if in.MaxKeys > 0 {
		li.MaxKeys = aws.Int32(in.MaxKeys)
	}
	if in.Delimiter != "" {
		li.Delimiter = aws.String(in.Delimiter)
	}
	if in.ContinuationToken != "" {
		li.ContinuationToken = aws.String(in.ContinuationToken)
	}
	out, err := c.api.ListObjectsV2(ctx, li)
	if err != nil {
		return ListOutput{}, fmt.Errorf("objectstore: list %q: %w", in.Prefix, err)
	}
	res := ListOutput{
		IsTruncated:           aws.ToBool(out.IsTruncated),
		NextContinuationToken: aws.ToString(out.NextContinuationToken),
	}
	for _, o := range out.Contents {
		obj := Object{
			Key:  aws.ToString(o.Key),
			ETag: trimETag(aws.ToString(o.ETag)),
		}
		if o.Size != nil {
			obj.Size = *o.Size
		}
		if o.LastModified != nil {
			obj.LastModified = *o.LastModified
		}
		res.Objects = append(res.Objects, obj)
	}
	for _, p := range out.CommonPrefixes {
		res.CommonPrefixes = append(res.CommonPrefixes, aws.ToString(p.Prefix))
	}
	return res, nil
}

func (c *S3Client) Delete(ctx context.Context, key string) error {
	_, err := c.api.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("objectstore: delete %q: %w", key, err)
	}
	return nil
}
