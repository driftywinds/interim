package download

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestURLRoundTrip(t *testing.T) {
	secret := strings.Repeat("s", 32)
	signer, err := NewSigner(secret, []string{"https://dl1.example.test", "https://dl2.example.test/"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0)
	raw, expires, err := signer.URL("files/abc", "Downloads.7z", 5*time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	if !expires.Equal(now.Add(5 * time.Minute)) {
		t.Fatalf("expires = %v", expires)
	}
	if !strings.HasPrefix(raw, "https://dl1.example.test/d/v1.") {
		t.Fatalf("URL = %q", raw)
	}

	parts := strings.Split(strings.TrimPrefix(raw, "https://dl1.example.test/d/"), ".")
	if len(parts) != 3 || parts[0] != "v1" {
		t.Fatalf("token = %q", raw)
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		t.Fatal("signature does not verify")
	}
	var decoded payload
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Key != "files/abc" || decoded.Filename != "Downloads.7z" || decoded.Expires != expires.Unix() {
		t.Fatalf("payload = %+v", decoded)
	}
}

func TestURLCanRotateWorkers(t *testing.T) {
	secret := strings.Repeat("s", 32)
	signer, err := NewSigner(secret, []string{"https://dl1.example.test", "https://dl2.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0)
	var urls []string
	for i := 0; i < 4; i++ {
		raw, _, err := signer.URL("files/abc", "a.txt", time.Minute, now)
		if err != nil {
			t.Fatal(err)
		}
		urls = append(urls, raw)
	}
	if !strings.HasPrefix(urls[0], "https://dl1.example.test/") ||
		!strings.HasPrefix(urls[1], "https://dl2.example.test/") ||
		!strings.HasPrefix(urls[2], "https://dl1.example.test/") {
		t.Fatalf("unexpected rotation: %v", urls)
	}
}

func TestNewSignerRejectsBadConfig(t *testing.T) {
	if _, err := NewSigner("short", []string{"https://dl.example.test"}); err == nil {
		t.Fatal("expected short secret error")
	}
	if _, err := NewSigner(strings.Repeat("s", 32), nil); err == nil {
		t.Fatal("expected empty URL error")
	}
}
