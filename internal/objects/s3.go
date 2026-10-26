// Package objects provides the object storage abstraction used by interim.
package objects

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/middleware"

	"interim/internal/config"
)

var (
	ErrNotFound     = errors.New("object not found")
	ErrNoSuchUpload = errors.New("multipart upload not found")
)

type MultipartPart struct {
	PartNumber int32
	ETag       string
}

type ObjectInfo struct {
	Key          string
	Size         int64
	ContentType  string
	ETag         string
	LastModified time.Time
}

type Store interface {
	Delete(ctx context.Context, key string) error
	Copy(ctx context.Context, srcKey, dstKey, contentType, contentDisposition string) error
	CreateMultipart(ctx context.Context, key, contentType, contentDisposition string) (string, error)
	PresignUploadPart(ctx context.Context, key, uploadID string, partNumber int, ttl time.Duration) (string, time.Time, error)
	CompleteMultipart(ctx context.Context, key, uploadID string, parts []MultipartPart) error
	AbortMultipart(ctx context.Context, key, uploadID string) error
	PresignPut(ctx context.Context, key, contentType, contentDisposition string, ttl time.Duration) (string, time.Time, error)
	PresignGet(ctx context.Context, key, filename string, ttl time.Duration) (string, time.Time, error)
	Head(ctx context.Context, key string) (ObjectInfo, error)
	List(ctx context.Context, prefix string) ([]ObjectInfo, error)
	Health(ctx context.Context) error
}

type S3Store struct {
	client       *s3.Client
	presigner    *s3.PresignClient
	putPresigner *s3.PresignClient
	bucket       string
	logger       *slog.Logger
}

func NewS3(ctx context.Context, cfg config.Config, logger *slog.Logger) (*S3Store, error) {
	loadOptions := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.S3.Region),
		awsconfig.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired),
		awsconfig.WithResponseChecksumValidation(aws.ResponseChecksumValidationWhenRequired),
		awsconfig.WithRetryMaxAttempts(4),
	}
	if cfg.S3.AccessKey != "" {
		loadOptions = append(loadOptions,
			awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
				cfg.S3.AccessKey, cfg.S3.SecretKey, "",
			)),
		)
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOptions...)
	if err != nil {
		return nil, fmt.Errorf("load S3 configuration: %w", err)
	}
	awsCfg.HTTPClient = s3HTTPClient()

	newClient := func(base aws.Config, endpoint string) *s3.Client {
		return s3.NewFromConfig(base, func(options *s3.Options) {
			if endpoint != "" {
				options.BaseEndpoint = aws.String(endpoint)
			}
			options.UsePathStyle = cfg.S3.PathStyle
			options.UseAccelerate = false
			options.UseARNRegion = false
			options.DisableMultiRegionAccessPoints = true
			options.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
			options.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
			// The Go SDK adds "Accept-Encoding: identity" and signs it. Some S3
			// implementations (and Cloudflare in front of them) normalize or
			// strip that header, which produces a SignatureDoesNotMatch. Remove
			// the middleware and disable transport compression instead, so no
			// Accept-Encoding header is sent at all.
			options.APIOptions = append(options.APIOptions, func(stack *middleware.Stack) error {
				_, _ = stack.Finalize.Remove("DisableAcceptEncodingGzip")
				return nil
			})
		})
	}

	client := newClient(awsCfg, cfg.S3.Endpoint)
	publicClient := client
	if cfg.S3.PublicEndpoint != cfg.S3.Endpoint {
		publicClient = newClient(awsCfg, cfg.S3.PublicEndpoint)
	}

	putCfg := awsCfg
	if cfg.S3.PutAccessKey != "" {
		putCfg.Credentials = credentials.NewStaticCredentialsProvider(
			cfg.S3.PutAccessKey, cfg.S3.PutSecretKey, "",
		)
	}
	putPublicClient := newClient(putCfg, cfg.S3.PublicEndpoint)

	store := &S3Store{
		client:       client,
		presigner:    s3.NewPresignClient(publicClient),
		putPresigner: s3.NewPresignClient(putPublicClient),
		bucket:       cfg.S3.Bucket,
		logger:       logger,
	}

	if err := store.ensureBucket(ctx, cfg.S3.AutoCreate); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *S3Store) ensureBucket(ctx context.Context, create bool) error {
	checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	_, err := s.client.HeadBucket(checkCtx, &s3.HeadBucketInput{Bucket: aws.String(s.bucket)})
	if err == nil {
		return nil
	}

	if !create {
		return fmt.Errorf("S3 bucket %q is not accessible (set S3_AUTO_CREATE_BUCKET=true to create it): %w", s.bucket, err)
	}

	input := &s3.CreateBucketInput{Bucket: aws.String(s.bucket)}
	if region := s.client.Options().Region; region != "" && region != "us-east-1" {
		input.CreateBucketConfiguration = &types.CreateBucketConfiguration{
			LocationConstraint: types.BucketLocationConstraint(region),
		}
	}
	_, createErr := s.client.CreateBucket(checkCtx, input)
	if createErr != nil && !isAlreadyExists(createErr) {
		return fmt.Errorf("create S3 bucket %q: %w", s.bucket, createErr)
	}
	s.logger.Info("created S3 bucket", "bucket", s.bucket)
	return nil
}

func isAlreadyExists(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "BucketAlreadyOwnedByYou", "BucketAlreadyExists":
			return true
		}
	}
	return false
}

func (s *S3Store) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("delete object: %w", err)
	}
	return nil
}

func (s *S3Store) Copy(ctx context.Context, srcKey, dstKey, contentType, contentDisposition string) error {
	input := &s3.CopyObjectInput{
		Bucket:            aws.String(s.bucket),
		Key:               aws.String(dstKey),
		CopySource:        aws.String(url.PathEscape(s.bucket + "/" + srcKey)),
		MetadataDirective: types.MetadataDirectiveReplace,
		ContentType:       aws.String(contentType),
	}
	if contentDisposition != "" {
		input.ContentDisposition = aws.String(contentDisposition)
	}
	if _, err := s.client.CopyObject(ctx, input); err != nil {
		return fmt.Errorf("copy object: %w", err)
	}
	return nil
}

func (s *S3Store) CreateMultipart(ctx context.Context, key, contentType, contentDisposition string) (string, error) {
	input := &s3.CreateMultipartUploadInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		ContentType: aws.String(contentType),
	}
	if contentDisposition != "" {
		input.ContentDisposition = aws.String(contentDisposition)
	}
	output, err := s.client.CreateMultipartUpload(ctx, input)
	if err != nil {
		return "", fmt.Errorf("create multipart upload: %w", err)
	}
	if output.UploadId == nil || *output.UploadId == "" {
		return "", errors.New("create multipart upload returned no upload id")
	}
	return *output.UploadId, nil
}

func (s *S3Store) PresignUploadPart(ctx context.Context, key, uploadID string, partNumber int, ttl time.Duration) (string, time.Time, error) {
	expiresAt := time.Now().UTC().Add(ttl)
	input := &s3.UploadPartInput{
		Bucket:     aws.String(s.bucket),
		Key:        aws.String(key),
		UploadId:   aws.String(uploadID),
		PartNumber: aws.Int32(int32(partNumber)),
	}
	presigned, err := s.putPresigner.PresignUploadPart(ctx, input, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("presign upload part: %w", err)
	}
	return presigned.URL, expiresAt, nil
}

func (s *S3Store) CompleteMultipart(ctx context.Context, key, uploadID string, parts []MultipartPart) error {
	completed := make([]types.CompletedPart, 0, len(parts))
	for _, part := range parts {
		completed = append(completed, types.CompletedPart{
			ETag:       aws.String(part.ETag),
			PartNumber: aws.Int32(part.PartNumber),
		})
	}
	_, err := s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{
			Parts: completed,
		},
	})
	if err != nil {
		if isNoSuchUpload(err) {
			return ErrNoSuchUpload
		}
		return fmt.Errorf("complete multipart upload: %w", err)
	}
	return nil
}

func (s *S3Store) AbortMultipart(ctx context.Context, key, uploadID string) error {
	_, err := s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
	})
	if err != nil && !isNoSuchUpload(err) {
		return fmt.Errorf("abort multipart upload: %w", err)
	}
	return nil
}

func (s *S3Store) PresignPut(ctx context.Context, key, contentType, contentDisposition string, ttl time.Duration) (string, time.Time, error) {
	expiresAt := time.Now().UTC().Add(ttl)
	input := &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		ContentType: aws.String(contentType),
	}
	if contentDisposition != "" {
		input.ContentDisposition = aws.String(contentDisposition)
	}
	presigned, err := s.putPresigner.PresignPutObject(ctx, input, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("presign upload object: %w", err)
	}
	return presigned.URL, expiresAt, nil
}

func (s *S3Store) PresignGet(ctx context.Context, key, filename string, ttl time.Duration) (string, time.Time, error) {
	expiresAt := time.Now().UTC().Add(ttl)
	input := &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}
	if disposition := AttachmentDisposition(filename); disposition != "" {
		input.ResponseContentDisposition = aws.String(disposition)
	}
	presigned, err := s.presigner.PresignGetObject(ctx, input, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("presign object: %w", err)
	}
	return presigned.URL, expiresAt, nil
}

func (s *S3Store) Head(ctx context.Context, key string) (ObjectInfo, error) {
	output, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return ObjectInfo{}, ErrNotFound
		}
		return ObjectInfo{}, fmt.Errorf("head object: %w", err)
	}
	info := ObjectInfo{Key: key}
	if output.ContentLength != nil {
		info.Size = *output.ContentLength
	}
	if output.ContentType != nil {
		info.ContentType = *output.ContentType
	}
	if output.ETag != nil {
		info.ETag = strings.Trim(*output.ETag, `"`)
	}
	if output.LastModified != nil {
		info.LastModified = output.LastModified.UTC()
	}
	return info, nil
}

func (s *S3Store) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})
	var objects []ObjectInfo
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list objects: %w", err)
		}
		for _, object := range page.Contents {
			info := ObjectInfo{Key: aws.ToString(object.Key)}
			if object.Size != nil {
				info.Size = *object.Size
			}
			if object.LastModified != nil {
				info.LastModified = object.LastModified.UTC()
			}
			objects = append(objects, info)
		}
	}
	return objects, nil
}

func (s *S3Store) Health(ctx context.Context) error {
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := s.client.HeadBucket(checkCtx, &s3.HeadBucketInput{Bucket: aws.String(s.bucket)})
	if err != nil {
		return fmt.Errorf("S3 health check: %w", err)
	}
	return nil
}

func isNoSuchUpload(err error) bool {
	if errors.Is(err, ErrNoSuchUpload) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode() == "NoSuchUpload"
	}
	return false
}

func isNotFound(err error) bool {
	if errors.Is(err, ErrNotFound) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return true
		}
	}
	return false
}

func AttachmentDisposition(filename string) string {
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": filename})
	if disposition == "" {
		return "attachment"
	}
	return disposition
}

func s3HTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext
	transport.TLSHandshakeTimeout = 10 * time.Second
	transport.ResponseHeaderTimeout = 30 * time.Second
	transport.IdleConnTimeout = 90 * time.Second
	transport.MaxIdleConns = 100
	transport.MaxIdleConnsPerHost = 16
	transport.DisableCompression = true
	return &http.Client{Transport: transport}
}

// SanitizeContentType normalizes an untrusted upload content type. Anything
// unknown becomes application/octet-stream.
func SanitizeContentType(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "application/octet-stream"
	}
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil || len(mediaType) > 255 {
		return "application/octet-stream"
	}
	return mediaType
}
