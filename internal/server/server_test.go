package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"interim/internal/config"
	"interim/internal/download"
	"interim/internal/objects"
	"interim/internal/store"
)

type fakeObject struct {
	data         []byte
	contentType  string
	lastModified time.Time
}

type fakeStore struct {
	mu      sync.Mutex
	objects map[string]fakeObject
}

func newFakeStore() *fakeStore {
	return &fakeStore{objects: make(map[string]fakeObject)}
}

func (f *fakeStore) putObject(key string, data []byte, modified time.Time) {
	f.mu.Lock()
	f.objects[key] = fakeObject{data: data, contentType: "application/octet-stream", lastModified: modified}
	f.mu.Unlock()
}

func (f *fakeStore) Delete(ctx context.Context, key string) error {
	f.mu.Lock()
	delete(f.objects, key)
	f.mu.Unlock()
	return nil
}

func (f *fakeStore) PresignPut(ctx context.Context, key, contentType, contentDisposition string, ttl time.Duration) (string, time.Time, error) {
	return "https://storage.test/put/" + url.PathEscape(key) + "?signature=test", time.Now().Add(ttl), nil
}

func (f *fakeStore) PresignGet(ctx context.Context, key, filename string, ttl time.Duration) (string, time.Time, error) {
	return "https://storage.test/signed/" + url.PathEscape(key), time.Now().Add(ttl), nil
}

func (f *fakeStore) Head(ctx context.Context, key string) (objects.ObjectInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	object, ok := f.objects[key]
	if !ok {
		return objects.ObjectInfo{}, objects.ErrNotFound
	}
	return objects.ObjectInfo{
		Key:          key,
		Size:         int64(len(object.data)),
		ContentType:  object.contentType,
		LastModified: object.lastModified,
	}, nil
}

func (f *fakeStore) List(ctx context.Context, prefix string) ([]objects.ObjectInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []objects.ObjectInfo
	for key, object := range f.objects {
		if len(prefix) == 0 || (len(key) >= len(prefix) && key[:len(prefix)] == prefix) {
			result = append(result, objects.ObjectInfo{
				Key:          key,
				Size:         int64(len(object.data)),
				ContentType:  object.contentType,
				LastModified: object.lastModified,
			})
		}
	}
	return result, nil
}

func (f *fakeStore) Health(ctx context.Context) error {
	return nil
}

func (f *fakeStore) exists(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objects[key]
	return ok
}

type testEnv struct {
	server *Server
	store  *store.Store
	fake   *fakeStore
	http   *httptest.Server
	client *http.Client
}

func newTestEnv(t *testing.T, maxStorage, maxFile int64) *testEnv {
	t.Helper()
	cfg := config.Config{
		PublicBaseURL:   "https://files.test",
		ListenAddr:      ":0",
		MaxStorage:      maxStorage,
		MaxFileSize:     maxFile,
		Retention:       7 * 24 * time.Hour,
		DownloadTTL:     5 * time.Minute,
		UploadURLTTL:    30 * time.Minute,
		UploadIntentTTL: 45 * time.Minute,
		SessionTTL:      time.Hour,
		CookieSecure:    false,
		S3: config.S3Config{
			Prefix:         "files/",
			Bucket:         "test",
			PublicEndpoint: "https://storage.test",
		},
	}
	st, err := store.Open(context.Background(), t.TempDir()+"/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.ResetForStartup(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake := newFakeStore()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	app, err := New(cfg, st, fake, logger)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(app.Handler())
	t.Cleanup(httpServer.Close)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar:           jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse },
	}
	return &testEnv{server: app, store: st, fake: fake, http: httpServer, client: client}
}

var csrfPattern = regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)

func extractCSRF(t *testing.T, body string) string {
	t.Helper()
	match := csrfPattern.FindStringSubmatch(body)
	if match == nil {
		t.Fatalf("csrf token not found in body: %s", body[:min(300, len(body))])
	}
	return match[1]
}

func getBody(t *testing.T, client *http.Client, url string) (int, string) {
	t.Helper()
	response, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(data)
}

func (e *testEnv) setupAdmin(t *testing.T) {
	t.Helper()
	status, body := getBody(t, e.client, e.http.URL+"/setup")
	if status != http.StatusOK {
		t.Fatalf("setup GET status = %d", status)
	}
	csrf := extractCSRF(t, body)
	form := url.Values{
		"csrf_token":       {csrf},
		"username":         {"admin"},
		"password":         {"correct-horse-battery"},
		"password_confirm": {"correct-horse-battery"},
	}
	response, err := e.client.PostForm(e.http.URL+"/setup", form)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("setup POST status = %d", response.StatusCode)
	}
	status, body = getBody(t, e.client, e.http.URL+"/admin")
	if status != http.StatusOK {
		t.Fatalf("admin GET status = %d: %s", status, body[:min(300, len(body))])
	}
}

func (e *testEnv) createKey(t *testing.T) (string, string) {
	t.Helper()
	response, err := e.client.Post(e.http.URL+"/api/v1/keys", "application/json", bytes.NewBufferString(`{"name":"test"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body struct {
		ID       string `json:"id"`
		UserHash string `json:"user_hash"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusCreated || body.UserHash == "" {
		t.Fatalf("key create status = %d, hash = %q", response.StatusCode, body.UserHash)
	}
	return body.ID, body.UserHash
}

type uploadResult struct {
	Status      int
	Body        []byte
	ID          string
	DeleteToken string
	PageURL     string
	Started     time.Time
}

func (e *testEnv) createIntent(t *testing.T, userHash, name string, size int, contentType string) (int, []byte, uploadIntentDTO) {
	t.Helper()
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	payload, _ := json.Marshal(map[string]any{
		"name":         name,
		"size":         size,
		"content_type": contentType,
	})
	request, err := http.NewRequest(http.MethodPost, e.http.URL+"/api/v1/uploads", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	if userHash != "" {
		request.Header.Set("Authorization", "Bearer "+userHash)
	}
	response, err := e.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	var intent uploadIntentDTO
	if response.StatusCode == http.StatusCreated {
		if err := json.Unmarshal(body, &intent); err != nil {
			t.Fatalf("decode upload intent: %v: %s", err, body)
		}
	}
	return response.StatusCode, body, intent
}

func (e *testEnv) completeIntent(t *testing.T, uploadID, uploadToken string) (int, []byte) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, e.http.URL+"/api/v1/uploads/"+uploadID+"/complete", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Upload-Token", uploadToken)
	response, err := e.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	return response.StatusCode, body
}

func (e *testEnv) upload(t *testing.T, userHash, name string, data []byte) uploadResult {
	t.Helper()
	start := time.Now()
	status, body, intent := e.createIntent(t, userHash, name, len(data), "application/octet-stream")
	result := uploadResult{Status: status, Body: body, Started: start}
	if status != http.StatusCreated {
		return result
	}
	e.fake.putObject("files/"+intent.UploadID, data, time.Now())

	completeStatus, completeBody := e.completeIntent(t, intent.UploadID, intent.UploadToken)
	result.Status = completeStatus
	result.Body = completeBody
	if completeStatus == http.StatusCreated || completeStatus == http.StatusOK {
		var completed struct {
			File struct {
				ID      string `json:"id"`
				PageURL string `json:"page_url"`
			} `json:"file"`
		}
		if err := json.Unmarshal(completeBody, &completed); err != nil {
			t.Fatalf("decode completed upload: %v: %s", err, completeBody)
		}
		result.ID = completed.File.ID
		result.PageURL = completed.File.PageURL
		result.DeleteToken = intent.DeleteToken
	}
	return result
}

func (e *testEnv) deleteFile(t *testing.T, id, userHash, deleteToken string) int {
	t.Helper()
	request, _ := http.NewRequest(http.MethodDelete, e.http.URL+"/api/v1/files/"+id, nil)
	if userHash != "" {
		request.Header.Set("Authorization", "Bearer "+userHash)
	}
	if deleteToken != "" {
		request.Header.Set("X-Delete-Token", deleteToken)
	}
	response, err := e.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	return response.StatusCode
}

func TestDownloadURLUsesConfiguredWorkers(t *testing.T) {
	signer, err := download.NewSigner(
		strings.Repeat("s", 32),
		[]string{"https://dl1.example.test", "https://dl2.example.test"},
	)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		cfg:       config.Config{DownloadTTL: 5 * time.Minute},
		downloads: signer,
		now:       func() time.Time { return time.Unix(1_700_000_000, 0) },
	}
	file := store.File{
		ObjectKey:    "files/abc",
		OriginalName: "Downloads.7z",
	}
	first, _, err := s.downloadURL(context.Background(), file)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := s.downloadURL(context.Background(), file)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first, "https://dl1.example.test/d/v1.") {
		t.Fatalf("first URL = %q", first)
	}
	if !strings.HasPrefix(second, "https://dl2.example.test/d/v1.") {
		t.Fatalf("second URL = %q", second)
	}
}

func TestPublicPagesAndAdminSetup(t *testing.T) {
	env := newTestEnv(t, 20<<20, 5<<20)

	status, body := getBody(t, env.client, env.http.URL+"/")
	if status != http.StatusOK {
		t.Fatalf("home status = %d", status)
	}
	for _, needle := range []string{"Every file is permanently deleted after 7 days", "Storage", "Create user hash"} {
		if !bytes.Contains([]byte(body), []byte(needle)) {
			t.Errorf("home missing %q", needle)
		}
	}
	response, err := env.client.Get(env.http.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	csp := response.Header.Get("Content-Security-Policy")
	response.Body.Close()
	if !strings.Contains(csp, "https://storage.test") {
		t.Errorf("CSP does not allow the S3 upload origin: %s", csp)
	}

	status, body = getBody(t, env.client, env.http.URL+"/api")
	if status != http.StatusOK || !bytes.Contains([]byte(body), []byte(env.server.cfg.PublicBaseURL+"/api/v1")) {
		t.Fatalf("api docs status = %d, base missing", status)
	}
	status, body = getBody(t, env.client, env.http.URL+"/api/openapi.json")
	if status != http.StatusOK || !json.Valid([]byte(body)) {
		t.Fatalf("openapi json status = %d valid = %v", status, json.Valid([]byte(body)))
	}

	response, err = env.client.Get(env.http.URL + "/admin")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/setup" {
		t.Fatalf("admin before setup = %d location=%q", response.StatusCode, response.Header.Get("Location"))
	}

	env.setupAdmin(t)
	status, body = getBody(t, env.client, env.http.URL+"/admin")
	if status != http.StatusOK || !bytes.Contains([]byte(body), []byte("All files")) {
		t.Fatalf("admin after setup status = %d", status)
	}
}

func TestAPIKeyUploadDownloadDelete(t *testing.T) {
	env := newTestEnv(t, 20<<20, 5<<20)
	keyID, userHash := env.createKey(t)
	if keyID == "" {
		t.Fatal("empty key id")
	}

	result := env.upload(t, userHash, "hello.txt", []byte("hello interim"))
	if result.Status != http.StatusCreated {
		t.Fatalf("upload status = %d: %s", result.Status, result.Body)
	}
	if result.ID == "" || result.DeleteToken == "" || result.PageURL == "" {
		t.Fatalf("incomplete upload response: %s", result.Body)
	}

	status, body := getBody(t, env.client, env.http.URL+"/f/"+result.ID)
	if status != http.StatusOK {
		t.Fatalf("file page status = %d", status)
	}
	if !bytes.Contains([]byte(body), []byte("Download File")) || !bytes.Contains([]byte(body), []byte("hello.txt")) {
		t.Error("file page missing download UI")
	}

	status, body = getBody(t, env.client, env.http.URL+"/api/v1/files/"+result.ID)
	if status != http.StatusOK || !bytes.Contains([]byte(body), []byte(`"name":"hello.txt"`)) {
		t.Fatalf("metadata status = %d: %s", status, body)
	}

	response, err := env.client.Post(env.http.URL+"/api/v1/files/"+result.ID+"/download", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	var download struct {
		URL      string `json:"url"`
		Filename string `json:"filename"`
	}
	json.NewDecoder(response.Body).Decode(&download)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || download.Filename != "hello.txt" || download.URL == "" {
		t.Fatalf("download status = %d payload=%+v", response.StatusCode, download)
	}

	response, err = env.client.Get(env.http.URL + "/f/" + result.ID + "/download")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") == "" {
		t.Fatalf("page download status = %d location = %q", response.StatusCode, response.Header.Get("Location"))
	}

	if code := env.deleteFile(t, result.ID, "", result.DeleteToken); code != http.StatusNoContent {
		t.Fatalf("delete token status = %d", code)
	}
	status, _ = getBody(t, env.client, env.http.URL+"/api/v1/files/"+result.ID)
	if status != http.StatusNotFound {
		t.Fatalf("metadata after delete = %d", status)
	}
}

func TestUploadUsesConfiguredRetention(t *testing.T) {
	env := newTestEnv(t, 20<<20, 5<<20)
	env.server.cfg.Retention = 12 * time.Hour
	status, home := getBody(t, env.client, env.http.URL+"/")
	if status != http.StatusOK || !bytes.Contains([]byte(home), []byte("after 12 hours")) {
		t.Fatalf("home did not reflect configured retention, status=%d", status)
	}
	status, statusBody := getBody(t, env.client, env.http.URL+"/api/v1/status")
	if status != http.StatusOK || !bytes.Contains([]byte(statusBody), []byte(`"retention_seconds":43200`)) || !bytes.Contains([]byte(statusBody), []byte(`"retention_human":"12 hours"`)) {
		t.Fatalf("status did not reflect configured retention: %s", statusBody)
	}
	result := env.upload(t, "", "short-lived.txt", []byte("short lived"))
	if result.Status != http.StatusCreated {
		t.Fatalf("upload status = %d: %s", result.Status, result.Body)
	}
	var payload struct {
		File struct {
			CreatedAt time.Time `json:"created_at"`
			ExpiresAt time.Time `json:"expires_at"`
		} `json:"file"`
	}
	if err := json.Unmarshal(result.Body, &payload); err != nil {
		t.Fatal(err)
	}
	lifetime := payload.File.ExpiresAt.Sub(payload.File.CreatedAt)
	if lifetime != 12*time.Hour {
		t.Fatalf("retention = %v, want 12h", lifetime)
	}
}

func TestOwnershipAndStorageQuota(t *testing.T) {
	env := newTestEnv(t, 100, 100)
	_, hashA := env.createKey(t)
	_, hashB := env.createKey(t)

	result := env.upload(t, hashA, "a.bin", make([]byte, 80))
	if result.Status != http.StatusCreated {
		t.Fatalf("upload A status = %d: %s", result.Status, result.Body)
	}
	if code := env.deleteFile(t, result.ID, hashB, ""); code != http.StatusNotFound {
		t.Fatalf("other key delete status = %d, want 404", code)
	}
	fill := env.upload(t, hashA, "b.bin", make([]byte, 20))
	if fill.Status != http.StatusCreated {
		t.Fatalf("fill upload status = %d: %s", fill.Status, fill.Body)
	}
	status, home := getBody(t, env.client, env.http.URL+"/")
	if status != http.StatusOK || !bytes.Contains([]byte(home), []byte("Storage maxed out")) {
		t.Fatalf("home did not show storage-full disclaimer, status=%d", status)
	}
	over := env.upload(t, hashA, "c.bin", []byte{1})
	if over.Status != http.StatusInsufficientStorage {
		t.Fatalf("quota upload status = %d, want 507: %s", over.Status, over.Body)
	}
	if code := env.deleteFile(t, result.ID, hashA, ""); code != http.StatusNoContent {
		t.Fatalf("owner delete status = %d", code)
	}
	if code := env.deleteFile(t, fill.ID, hashA, ""); code != http.StatusNoContent {
		t.Fatalf("fill delete status = %d", code)
	}
}

func TestUnlimitedStorage(t *testing.T) {
	env := newTestEnv(t, 0, 5<<20)
	result := env.upload(t, "", "unlimited.bin", []byte("no cap"))
	if result.Status != http.StatusCreated {
		t.Fatalf("upload status = %d: %s", result.Status, result.Body)
	}

	status, home := getBody(t, env.client, env.http.URL+"/")
	if status != http.StatusOK {
		t.Fatalf("home status = %d", status)
	}
	if bytes.Contains([]byte(home), []byte("<legend>Storage</legend>")) {
		t.Error("home should hide the storage panel when the cap is disabled")
	}
	if bytes.Contains([]byte(home), []byte("Storage maxed out")) {
		t.Error("home should hide the capacity warning when the cap is disabled")
	}

	status, body := getBody(t, env.client, env.http.URL+"/api/v1/status")
	if status != http.StatusOK || !bytes.Contains([]byte(body), []byte(`"unlimited":true`)) {
		t.Fatalf("status did not report unlimited storage: %d %s", status, body)
	}
}

func TestExpiryAndCleanup(t *testing.T) {
	env := newTestEnv(t, 20<<20, 5<<20)
	result := env.upload(t, "", "gone.txt", []byte("temporary"))
	if result.Status != http.StatusCreated {
		t.Fatalf("upload status = %d: %s", result.Status, result.Body)
	}

	env.server.now = func() time.Time { return time.Now().Add(8 * 24 * time.Hour) }
	status, _ := getBody(t, env.client, env.http.URL+"/f/"+result.ID)
	if status != http.StatusGone {
		t.Fatalf("expired page status = %d", status)
	}
	status, _ = getBody(t, env.client, env.http.URL+"/api/v1/files/"+result.ID)
	if status != http.StatusGone {
		t.Fatalf("expired metadata status = %d", status)
	}
	env.server.MaintenanceOnce(context.Background())
	if env.fake.exists("files/" + result.ID) {
		t.Fatal("expired object was not removed")
	}
	if _, err := env.store.FileByID(context.Background(), result.ID); err == nil {
		t.Fatal("expired row was not removed")
	}
}

func TestAdminFileDelete(t *testing.T) {
	env := newTestEnv(t, 20<<20, 5<<20)
	env.setupAdmin(t)
	_, userHash := env.createKey(t)
	request, _ := http.NewRequest(http.MethodGet, env.http.URL+"/api/v1/keys/me", nil)
	request.Header.Set("Authorization", "Bearer "+userHash)
	response, err := env.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("key info status = %d", response.StatusCode)
	}
	result := env.upload(t, userHash, "admin-delete.txt", []byte("bye"))
	if result.Status != http.StatusCreated {
		t.Fatalf("upload status = %d", result.Status)
	}
	status, body := getBody(t, env.client, env.http.URL+"/admin")
	if status != http.StatusOK || !bytes.Contains([]byte(body), []byte("admin-delete.txt")) {
		t.Fatalf("admin missing uploaded file, status=%d", status)
	}
	csrf := extractCSRF(t, body)
	form := url.Values{"csrf_token": {csrf}, "ids": {result.ID}}
	response, err = env.client.PostForm(env.http.URL+"/admin/files/delete", form)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("admin delete status = %d", response.StatusCode)
	}
	status, _ = getBody(t, env.client, env.http.URL+"/api/v1/files/"+result.ID)
	if status != http.StatusNotFound {
		t.Fatalf("file after admin delete = %d", status)
	}
}

func TestDirectUploadIntentAndIdempotentCompletion(t *testing.T) {
	env := newTestEnv(t, 20<<20, 5<<20)
	status, body, intent := env.createIntent(t, "", "direct.txt", 11, "text/plain")
	if status != http.StatusCreated {
		t.Fatalf("create intent status = %d: %s", status, body)
	}
	if intent.UploadID == "" || intent.UploadToken == "" || intent.DeleteToken == "" || intent.URL == "" {
		t.Fatalf("incomplete intent: %+v", intent)
	}
	if intent.Method != "PUT" || intent.Headers["Content-Type"] != "text/plain" || intent.Headers["Content-Disposition"] == "" {
		t.Fatalf("unexpected upload contract: %+v", intent)
	}
	usage, err := env.store.StorageUsage(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if usage.Reserved != 11 || usage.Used != 0 {
		t.Fatalf("after intent usage = %+v", usage)
	}

	env.fake.putObject("files/"+intent.UploadID, []byte("hello world"), time.Now())
	completeStatus, completeBody := env.completeIntent(t, intent.UploadID, intent.UploadToken)
	if completeStatus != http.StatusCreated {
		t.Fatalf("complete status = %d: %s", completeStatus, completeBody)
	}
	retryStatus, retryBody := env.completeIntent(t, intent.UploadID, intent.UploadToken)
	if retryStatus != http.StatusOK {
		t.Fatalf("idempotent complete status = %d: %s", retryStatus, retryBody)
	}
	usage, err = env.store.StorageUsage(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if usage.Used != 11 || usage.Reserved != 0 || usage.Files != 1 {
		t.Fatalf("after complete usage = %+v", usage)
	}
}

func TestUploadRejectsActualSizeMismatch(t *testing.T) {
	env := newTestEnv(t, 20<<20, 5<<20)
	status, body, intent := env.createIntent(t, "", "liar.bin", 10, "application/octet-stream")
	if status != http.StatusCreated {
		t.Fatalf("create intent status = %d: %s", status, body)
	}
	env.fake.putObject("files/"+intent.UploadID, make([]byte, 20), time.Now())
	completeStatus, completeBody := env.completeIntent(t, intent.UploadID, intent.UploadToken)
	if completeStatus != http.StatusRequestEntityTooLarge {
		t.Fatalf("complete status = %d: %s", completeStatus, completeBody)
	}
	if env.fake.exists("files/" + intent.UploadID) {
		t.Fatal("oversized object was not deleted")
	}
	stored, err := env.store.UploadIntentByID(context.Background(), intent.UploadID)
	if err != nil || stored.State != "failed" {
		t.Fatalf("intent state = %+v, err = %v", stored, err)
	}
	usage, _ := env.store.StorageUsage(context.Background(), time.Now())
	if usage.Reserved != 0 || usage.Used != 0 {
		t.Fatalf("usage after failed upload = %+v", usage)
	}
}

func TestAbortUploadDeletesObjectAndReservation(t *testing.T) {
	env := newTestEnv(t, 20<<20, 5<<20)
	status, body, intent := env.createIntent(t, "", "cancel.bin", 10, "")
	if status != http.StatusCreated {
		t.Fatalf("create intent status = %d: %s", status, body)
	}
	env.fake.putObject("files/"+intent.UploadID, make([]byte, 10), time.Now())
	request, _ := http.NewRequest(http.MethodDelete, env.http.URL+"/api/v1/uploads/"+intent.UploadID, nil)
	request.Header.Set("X-Upload-Token", intent.UploadToken)
	response, err := env.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("abort status = %d", response.StatusCode)
	}
	if env.fake.exists("files/" + intent.UploadID) {
		t.Fatal("aborted object still exists")
	}
	stored, err := env.store.UploadIntentByID(context.Background(), intent.UploadID)
	if err != nil || stored.State != "aborted" {
		t.Fatalf("intent state = %+v, err = %v", stored, err)
	}
	usage, _ := env.store.StorageUsage(context.Background(), time.Now())
	if usage.Reserved != 0 || usage.Used != 0 {
		t.Fatalf("usage after abort = %+v", usage)
	}
}

func TestJanitorExpiresAbandonedUpload(t *testing.T) {
	env := newTestEnv(t, 20<<20, 5<<20)
	status, body, intent := env.createIntent(t, "", "abandoned.bin", 10, "")
	if status != http.StatusCreated {
		t.Fatalf("create intent status = %d: %s", status, body)
	}
	env.fake.putObject("files/"+intent.UploadID, make([]byte, 10), time.Now())
	env.server.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	env.server.MaintenanceOnce(context.Background())
	if env.fake.exists("files/" + intent.UploadID) {
		t.Fatal("janitor did not delete abandoned object")
	}
	stored, err := env.store.UploadIntentByID(context.Background(), intent.UploadID)
	if err != nil || stored.State != "expired" {
		t.Fatalf("intent state = %+v, err = %v", stored, err)
	}
	usage, _ := env.store.StorageUsage(context.Background(), env.server.now())
	if usage.Reserved != 0 || usage.Used != 0 {
		t.Fatalf("usage after janitor = %+v", usage)
	}
}

func TestJanitorOrphanSweepAtStartup(t *testing.T) {
	env := newTestEnv(t, 20<<20, 5<<20)
	env.fake.putObject("files/orphan-object", []byte("orphan"), time.Now().Add(-time.Hour))
	status, body, active := env.createIntent(t, "", "active.bin", 4, "")
	if status != http.StatusCreated {
		t.Fatalf("create intent status = %d: %s", status, body)
	}
	env.fake.putObject("files/"+active.UploadID, []byte("live"), time.Now())
	env.server.StartupMaintenance(context.Background())
	if env.fake.exists("files/orphan-object") {
		t.Fatal("startup sweep did not delete orphan object")
	}
	if !env.fake.exists("files/" + active.UploadID) {
		t.Fatal("startup sweep deleted an active pending upload")
	}
}
