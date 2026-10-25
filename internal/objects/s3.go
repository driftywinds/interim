// Package objects provides the object storage abstraction used by interim.
package objects

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"interim/internal/config"
)

type Store interface {
	Put(ctx context.Context, key string, body io.Reader, contentType, filename string) error
	Delete(ctx context.Context, key string) error
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, time.Time, error)
	Health(ctx context.Context) error
}

type S3Store struct {
	client    *s3.Client
	presigner *s3.PresignClient
	uploader  *manager.Uploader
	bucket    string
	logger    *slog.Logger
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

	newClient := func(endpoint string) *s3.Client {
		return s3.NewFromConfig(awsCfg, func(options *s3.Options) {
			if endpoint != "" {
				options.BaseEndpoint = aws.String(endpoint)
			}
			options.UsePathStyle = cfg.S3.PathStyle
			options.UseAccelerate = false
			options.UseARNRegion = false
			options.DisableMultiRegionAccessPoints = true
			options.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
			options.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
		})
	}

	client := newClient(cfg.S3.Endpoint)
	publicClient := client
	if cfg.S3.PublicEndpoint != cfg.S3.Endpoint {
		publicClient = newClient(cfg.S3.PublicEndpoint)
	}

	store := &S3Store{
		client:    client,
		presigner: s3.NewPresignClient(publicClient),
		bucket:    cfg.S3.Bucket,
		logger:    logger,
		uploader: manager.NewUploader(client, func(uploader *manager.Uploader) {
			uploader.PartSize = cfg.S3.PartSize
			uploader.Concurrency = cfg.S3.Concurrency
			uploader.LeavePartsOnError = false
			uploader.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		}),
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

func (s *S3Store) Put(ctx context.Context, key string, body io.Reader, contentType, filename string) error {
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": filename})
	if disposition == "" {
		disposition = "attachment"
	}
	_, err := s.uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket:             aws.String(s.bucket),
		Key:                aws.String(key),
		Body:               body,
		ContentType:        aws.String(contentType),
		ContentDisposition: aws.String(disposition),
	})
	if err != nil {
		return fmt.Errorf("upload object: %w", err)
	}
	return nil
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

func (s *S3Store) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, time.Time, error) {
	expiresAt := time.Now().UTC().Add(ttl)
	presigned, err := s.presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("presign object: %w", err)
	}
	return presigned.URL, expiresAt, nil
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
