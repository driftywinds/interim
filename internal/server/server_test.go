package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"sync"
	"testing"
	"time"

	"interim/internal/config"
	"interim/internal/store"
)

type fakeStore struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newFakeStore() *fakeStore {
	return &fakeStore{objects: make(map[string][]byte)}
}

func (f *fakeStore) Put(ctx context.Context, key string, body io.Reader, contentType, filename string) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.objects[key] = data
	f.mu.Unlock()
	return nil
}

func (f *fakeStore) Delete(ctx context.Context, key string) error {
	f.mu.Lock()
	delete(f.objects, key)
	f.mu.Unlock()
	return nil
}

func (f *fakeStore) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, time.Time, error) {
	return "https://storage.test/signed/" + url.PathEscape(key), time.Now().Add(ttl), nil
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
		PublicBaseURL: "https://files.test",
		ListenAddr:    ":0",
		MaxStorage:    maxStorage,
		MaxFileSize:   maxFile,
		Retention:     7 * 24 * time.Hour,
		DownloadTTL:   5 * time.Minute,
		SessionTTL:    time.Hour,
		CookieSecure:  false,
		S3: config.S3Config{
			Prefix: "files/",
			Bucket: "test",
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

func (e *testEnv) upload(t *testing.T, userHash, name string, data []byte) uploadResult {
	t.Helper()
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, err := writer.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	request, err := http.NewRequest(http.MethodPost, e.http.URL+"/api/v1/files", &buf)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("X-File-Size", strconv.Itoa(len(data)))
	if userHash != "" {
		request.Header.Set("Authorization", "Bearer "+userHash)
	}
	response, err := e.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	result := uploadResult{Status: response.StatusCode, Body: body, Started: start}
	if response.StatusCode == http.StatusCreated {
		var payload struct {
			ID          string `json:"id"`
			DeleteToken string `json:"delete_token"`
			PageURL     string `json:"page_url"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("decode upload response: %v: %s", err, body)
		}
		result.ID = payload.ID
		result.DeleteToken = payload.DeleteToken
		result.PageURL = payload.PageURL
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

	status, body = getBody(t, env.client, env.http.URL+"/api")
	if status != http.StatusOK || !bytes.Contains([]byte(body), []byte(env.server.cfg.PublicBaseURL+"/api/v1")) {
		t.Fatalf("api docs status = %d, base missing", status)
	}
	status, body = getBody(t, env.client, env.http.URL+"/api/openapi.json")
	if status != http.StatusOK || !json.Valid([]byte(body)) {
		t.Fatalf("openapi json status = %d valid = %v", status, json.Valid([]byte(body)))
	}

	response, err := env.client.Get(env.http.URL + "/admin")
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
		CreatedAt time.Time `json:"created_at"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(result.Body, &payload); err != nil {
		t.Fatal(err)
	}
	lifetime := payload.ExpiresAt.Sub(payload.CreatedAt)
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
