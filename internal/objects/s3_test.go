package objects

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestS3LongClientHasNoResponseHeaderTimeout(t *testing.T) {
	client := s3LongHTTPClient()
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T", client.Transport)
	}
	if transport.ResponseHeaderTimeout != 0 {
		t.Fatalf("long-client response header timeout = %v", transport.ResponseHeaderTimeout)
	}
	if s3HTTPClient().Transport.(*http.Transport).ResponseHeaderTimeout != 30*time.Second {
		t.Fatal("short-client response header timeout changed unexpectedly")
	}
}

func TestPresignPutSignsContentLength(t *testing.T) {
	cfg := aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("access", "secret", ""),
	}
	client := s3.NewFromConfig(cfg, func(options *s3.Options) {
		options.UsePathStyle = true
		options.BaseEndpoint = aws.String("https://storage.example.test")
	})
	store := &S3Store{
		putPresigner: s3.NewPresignClient(client),
		bucket:       "interim",
	}

	signed, _, err := store.PresignPut(
		context.Background(),
		"files/uploads/test",
		"application/octet-stream",
		`attachment; filename="test.bin"`,
		1234,
		time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(signed)
	if err != nil {
		t.Fatal(err)
	}
	headers := parsed.Query().Get("X-Amz-SignedHeaders")
	if !strings.Contains(headers, "content-length") {
		t.Fatalf("signed headers = %q, want content-length", headers)
	}
}
