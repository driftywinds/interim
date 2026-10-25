// Package download creates short-lived, worker-proxied download URLs.
package download

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

const tokenVersion = 1

type payload struct {
	Version  int    `json:"v"`
	Key      string `json:"k"`
	Filename string `json:"n"`
	Expires  int64  `json:"e"`
}

// Signer creates HMAC-signed download tokens and rotates across the configured
// worker base URLs. Every worker must use the same token secret.
type Signer struct {
	secret []byte
	bases  []string
	next   atomic.Uint64
}

func NewSigner(secret string, bases []string) (*Signer, error) {
	secret = strings.TrimSpace(secret)
	if len(secret) < 32 {
		return nil, errors.New("download worker secret must be at least 32 characters")
	}
	if len(bases) == 0 {
		return nil, errors.New("at least one download worker URL is required")
	}
	clean := make([]string, 0, len(bases))
	for _, base := range bases {
		base = strings.TrimSpace(base)
		base = strings.TrimSuffix(base, "/")
		if base == "" {
			return nil, errors.New("download worker URL cannot be empty")
		}
		clean = append(clean, base)
	}
	return &Signer{secret: []byte(secret), bases: clean}, nil
}

// URL returns a URL for one worker and its expiry. The worker verifies the
// token with the same secret and fetches the private object from B2.
func (s *Signer) URL(key, filename string, ttl time.Duration, now time.Time) (string, time.Time, error) {
	if key == "" {
		return "", time.Time{}, errors.New("download object key is empty")
	}
	if ttl <= 0 {
		return "", time.Time{}, errors.New("download token TTL must be positive")
	}
	if strings.TrimSpace(filename) == "" {
		filename = "file"
	}

	expires := now.UTC().Add(ttl)
	body, err := json.Marshal(payload{
		Version:  tokenVersion,
		Key:      key,
		Filename: filename,
		Expires:  expires.Unix(),
	})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("encode download token: %w", err)
	}

	encoded := base64.RawURLEncoding.EncodeToString(body)
	mac := hmac.New(sha256.New, s.secret)
	_, _ = mac.Write(body)
	signature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	index := s.next.Add(1) - 1
	base := s.bases[index%uint64(len(s.bases))]
	token := fmt.Sprintf("v%d.%s.%s", tokenVersion, encoded, signature)
	return base + "/d/" + token, expires, nil
}
