package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strconv"
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

type fakePart struct {
	data []byte
	etag string
}

type fakeMultipart struct {
	key         string
	contentType string
	initiated   time.Time
	parts       map[int]fakePart
}

type fakeStore struct {
	mu            sync.Mutex
	objects       map[string]fakeObject
	multipart     map[string]*fakeMultipart
	nextMultipart int
	abortErr      error
	healthCalls   int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		objects:   make(map[string]fakeObject),
		multipart: make(map[string]*fakeMultipart),
	}
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

func (f *fakeStore) CreateMultipart(ctx context.Context, key, contentType, contentDisposition string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextMultipart++
	id := "mp-" + strconv.Itoa(f.nextMultipart)
	f.multipart[id] = &fakeMultipart{
		key:         key,
		contentType: contentType,
		initiated:   time.Now(),
		parts:       make(map[int]fakePart),
	}
	return id, nil
}

func (f *fakeStore) PresignUploadPart(ctx context.Context, key, uploadID string, partNumber int, contentLength int64, ttl time.Duration) (string, time.Time, error) {
	return "https://storage.test/part/" + url.PathEscape(key) + "/" + url.QueryEscape(uploadID) + "/" + strconv.Itoa(partNumber), time.Now().Add(ttl), nil
}

func (f *fakeStore) ListParts(ctx context.Context, key, uploadID string) ([]objects.MultipartPartInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	upload, ok := f.multipart[uploadID]
	if !ok || upload.key != key {
		return nil, objects.ErrNoSuchUpload
	}
	parts := make([]objects.MultipartPartInfo, 0, len(upload.parts))
	for number, part := range upload.parts {
		parts = append(parts, objects.MultipartPartInfo{
			PartNumber: int32(number),
			ETag:       part.etag,
			Size:       int64(len(part.data)),
		})
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].PartNumber < parts[j].PartNumber })
	return parts, nil
}

func (f *fakeStore) ListMultipartUploads(ctx context.Context, prefix string) ([]objects.MultipartUploadInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var uploads []objects.MultipartUploadInfo
	for id, upload := range f.multipart {
		if strings.HasPrefix(upload.key, prefix) {
			uploads = append(uploads, objects.MultipartUploadInfo{Key: upload.key, UploadID: id, Initiated: upload.initiated})
		}
	}
	return uploads, nil
}

func (f *fakeStore) CompleteMultipart(ctx context.Context, key, uploadID string, parts []objects.MultipartPart) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	upload, ok := f.multipart[uploadID]
	if !ok || upload.key != key {
		return objects.ErrNoSuchUpload
	}
	var data []byte
	for _, part := range parts {
		stored, ok := upload.parts[int(part.PartNumber)]
		if !ok || stored.etag != part.ETag {
			return errors.New("fake multipart part missing or etag mismatch")
		}
		data = append(data, stored.data...)
	}
	f.objects[key] = fakeObject{
		data:         data,
		contentType:  upload.contentType,
		lastModified: time.Now(),
	}
	delete(f.multipart, uploadID)
	return nil
}

func (f *fakeStore) AbortMultipart(ctx context.Context, key, uploadID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.abortErr != nil {
		return f.abortErr
	}
	delete(f.multipart, uploadID)
	return nil
}

func (f *fakeStore) multipartIDFor(key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, upload := range f.multipart {
		if upload.key == key {
			return id
		}
	}
	return ""
}

func (f *fakeStore) putPart(uploadID string, partNumber int, data []byte) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	upload := f.multipart[uploadID]
	if upload == nil {
		return ""
	}
	etag := `"etag-` + strconv.Itoa(partNumber) + "-" + strconv.Itoa(len(data)) + `"`
	upload.parts[partNumber] = fakePart{data: append([]byte(nil), data...), etag: etag}
	return etag
}

func (f *fakeStore) Copy(ctx context.Context, srcKey, dstKey, contentType, contentDisposition string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	object, ok := f.objects[srcKey]
	if !ok {
		return objects.ErrNotFound
	}
	f.objects[dstKey] = fakeObject{
		data:         append([]byte(nil), object.data...),
		contentType:  contentType,
		lastModified: time.Now(),
	}
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
	f.mu.Lock()
	f.healthCalls++
	f.mu.Unlock()
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
		PublicBaseURL:              "https://files.test",
		ListenAddr:                 ":0",
		MaxStorage:                 maxStorage,
		MaxFileSize:                maxFile,
		Retention:                  7 * 24 * time.Hour,
		DownloadTTL:                5 * time.Minute,
		UploadURLTTL:               30 * time.Minute,
		UploadIntentTTL:            45 * time.Minute,
		MaxFiles:                   100000,
		MaxPendingUploadsPerClient: 2,
		MaxReservedBytesPerClient:  maxFile,
		MultipartThreshold:         64 << 20,
		MultipartPartSize:          16 << 20,
		MultipartConcurrency:       3,
		PublicUploadsEnabled:       true,
		SessionTTL:                 time.Hour,
		CookieSecure:               false,
		SetupToken:                 "0123456789abcdef0123456789abcdef",
		SessionSecret:              []byte("0123456789abcdef0123456789abcdef"),
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

var (
	csrfPattern   = regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)
	newKeyPattern = regexp.MustCompile(`id="new-key-value">([^<]+)</code>`)
)

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
		"setup_token":      {e.server.cfg.SetupToken},
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

func stagingKey(uploadID string) string {
	return "files/uploads/" + uploadID
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

func (e *testEnv) multipartPartURLs(t *testing.T, intent uploadIntentDTO, partNumbers []int) []uploadPartURLDTO {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"part_numbers": partNumbers})
	request, err := http.NewRequest(http.MethodPost, e.http.URL+"/api/v1/uploads/"+intent.UploadID+"/parts", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Upload-Token", intent.UploadToken)
	response, err := e.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("part URLs status = %d: %s", response.StatusCode, data)
	}
	var payload uploadPartURLsDTO
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	return payload.Parts
}

func (e *testEnv) reportPart(t *testing.T, intent uploadIntentDTO, partNumber int, etag string, size int) int {
	t.Helper()
	body := `{"etag":` + strconv.Quote(etag) + `,"size":` + strconv.Itoa(size) + `}`
	request, err := http.NewRequest(http.MethodPut, e.http.URL+"/api/v1/uploads/"+intent.UploadID+"/parts/"+strconv.Itoa(partNumber), bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Upload-Token", intent.UploadToken)
	response, err := e.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	return response.StatusCode
}

func (e *testEnv) uploadStatus(t *testing.T, intent uploadIntentDTO) (int, uploadStatusDTO) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, e.http.URL+"/api/v1/uploads/"+intent.UploadID, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Upload-Token", intent.UploadToken)
	response, err := e.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var payload uploadStatusDTO
	if response.StatusCode == http.StatusOK {
		if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
	}
	return response.StatusCode, payload
}

func (e *testEnv) upload(t *testing.T, userHash, name string, data []byte) uploadResult {
	t.Helper()
	start := time.Now()
	status, body, intent := e.createIntent(t, userHash, name, len(data), "application/octet-stream")
	result := uploadResult{Status: status, Body: body, Started: start}
	if status != http.StatusCreated {
		return result
	}
	e.fake.putObject(stagingKey(intent.UploadID), data, time.Now())

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

func TestSetupRequiresSetupToken(t *testing.T) {
	env := newTestEnv(t, 20<<20, 5<<20)
	status, body := getBody(t, env.client, env.http.URL+"/setup")
	if status != http.StatusOK || !bytes.Contains([]byte(body), []byte(`name="setup_token"`)) {
		t.Fatalf("setup page status = %d, token field missing", status)
	}
	csrf := extractCSRF(t, body)
	form := url.Values{
		"csrf_token":       {csrf},
		"setup_token":      {"wrong-token"},
		"username":         {"admin"},
		"password":         {"correct-horse-battery"},
		"password_confirm": {"correct-horse-battery"},
	}
	response, err := env.client.PostForm(env.http.URL+"/setup", form)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong setup token status = %d", response.StatusCode)
	}
	if hasAdmin, err := env.store.HasAdmin(context.Background()); err != nil || hasAdmin {
		t.Fatalf("admin should not exist: %v", err)
	}
	env.setupAdmin(t)
}

func TestCompletionCopiesToLiveObject(t *testing.T) {
	env := newTestEnv(t, 20<<20, 5<<20)
	result := env.upload(t, "", "payload.txt", []byte("hello"))
	if result.Status != http.StatusCreated {
		t.Fatalf("upload status = %d: %s", result.Status, result.Body)
	}
	file, err := env.store.FileByID(context.Background(), result.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(file.ObjectKey, "files/objects/") {
		t.Fatalf("live object key = %q", file.ObjectKey)
	}
	if strings.Contains(file.ObjectKey, result.ID) {
		t.Fatalf("live key is derivable from upload id: %q", file.ObjectKey)
	}
	if env.fake.exists(stagingKey(result.ID)) {
		t.Fatal("staging object was not removed after commit")
	}
	if !env.fake.exists(file.ObjectKey) {
		t.Fatal("live object is missing")
	}

	// Recreating the old staging key must not affect the committed file.
	env.fake.putObject(stagingKey(result.ID), []byte("tampered and longer"), time.Now())
	info, err := env.fake.Head(context.Background(), file.ObjectKey)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != int64(len("hello")) {
		t.Fatalf("live object size = %d", info.Size)
	}
}

func TestMultipartUploadResumeAndComplete(t *testing.T) {
	env := newTestEnv(t, 20<<20, 20)
	env.server.cfg.MultipartEnabled = true
	env.server.cfg.MultipartThreshold = 1
	env.server.cfg.MultipartPartSize = 3
	env.server.cfg.MultipartConcurrency = 2

	status, body, intent := env.createIntent(t, "", "multipart.bin", 7, "application/octet-stream")
	if status != http.StatusCreated {
		t.Fatalf("create multipart intent = %d: %s", status, body)
	}
	if intent.Mode != "multipart" || intent.URL != "" || intent.PartSize != 3 || intent.PartCount != 3 || intent.PartsURL == "" || intent.StatusURL == "" {
		t.Fatalf("unexpected multipart intent: %+v", intent)
	}
	if urls := env.multipartPartURLs(t, intent, []int{1, 2, 3}); len(urls) != 3 {
		t.Fatalf("part URL count = %d", len(urls))
	}

	multipartID := env.fake.multipartIDFor(stagingKey(intent.UploadID))
	if multipartID == "" {
		t.Fatal("fake multipart upload was not created")
	}
	partData := [][]byte{[]byte("abc"), []byte("def"), []byte("g")}
	for i, data := range partData {
		partNumber := i + 1
		etag := env.fake.putPart(multipartID, partNumber, data)
		if etag == "" {
			t.Fatal("fake part upload failed")
		}
		if code := env.reportPart(t, intent, partNumber, etag, len(data)); code != http.StatusNoContent {
			t.Fatalf("report part %d status = %d", partNumber, code)
		}
	}

	status, state := env.uploadStatus(t, intent)
	if status != http.StatusOK || state.Mode != "multipart" || len(state.CompletedParts) != 3 {
		t.Fatalf("upload status = %d %+v", status, state)
	}
	completeStatus, completeBody := env.completeIntent(t, intent.UploadID, intent.UploadToken)
	if completeStatus != http.StatusCreated {
		t.Fatalf("multipart complete = %d: %s", completeStatus, completeBody)
	}
	file, err := env.store.FileByID(context.Background(), intent.UploadID)
	if err != nil {
		t.Fatal(err)
	}
	if file.Size != 7 || file.ObjectKey != stagingKey(intent.UploadID) {
		t.Fatalf("committed multipart file = %+v", file)
	}
	if !env.fake.exists(stagingKey(intent.UploadID)) {
		t.Fatal("multipart live object is missing")
	}
	if env.fake.multipartIDFor(stagingKey(intent.UploadID)) != "" {
		t.Fatal("multipart upload was not finalized")
	}
	if parts, err := env.store.UploadParts(context.Background(), intent.UploadID); err != nil || len(parts) != 0 {
		t.Fatalf("upload parts after commit = %+v, %v", parts, err)
	}
}

func TestMultipartCompletionRequiresAllParts(t *testing.T) {
	env := newTestEnv(t, 20<<20, 20)
	env.server.cfg.MultipartEnabled = true
	env.server.cfg.MultipartThreshold = 1
	env.server.cfg.MultipartPartSize = 3

	status, body, intent := env.createIntent(t, "", "partial.bin", 7, "application/octet-stream")
	if status != http.StatusCreated {
		t.Fatalf("create multipart intent = %d: %s", status, body)
	}
	multipartID := env.fake.multipartIDFor(stagingKey(intent.UploadID))
	if multipartID == "" {
		t.Fatal("fake multipart upload was not created")
	}
	etag := env.fake.putPart(multipartID, 1, []byte("abc"))
	if code := env.reportPart(t, intent, 1, etag, 3); code != http.StatusNoContent {
		t.Fatalf("report part status = %d", code)
	}
	completeStatus, completeBody := env.completeIntent(t, intent.UploadID, intent.UploadToken)
	if completeStatus != http.StatusConflict || !bytes.Contains(completeBody, []byte(`"upload_parts_incomplete"`)) {
		t.Fatalf("incomplete multipart complete = %d: %s", completeStatus, completeBody)
	}
	status, state := env.uploadStatus(t, intent)
	if status != http.StatusOK || len(state.CompletedParts) != 1 {
		t.Fatalf("resume status = %d %+v", status, state)
	}
}

func TestMultipartAbort(t *testing.T) {
	env := newTestEnv(t, 20<<20, 20)
	env.server.cfg.MultipartEnabled = true
	env.server.cfg.MultipartThreshold = 1
	env.server.cfg.MultipartPartSize = 3

	status, body, intent := env.createIntent(t, "", "cancel.bin", 7, "application/octet-stream")
	if status != http.StatusCreated {
		t.Fatalf("create multipart intent = %d: %s", status, body)
	}
	multipartID := env.fake.multipartIDFor(stagingKey(intent.UploadID))
	if multipartID == "" {
		t.Fatal("fake multipart upload was not created")
	}
	etag := env.fake.putPart(multipartID, 1, []byte("abc"))
	if code := env.reportPart(t, intent, 1, etag, 3); code != http.StatusNoContent {
		t.Fatalf("report part status = %d", code)
	}
	request, _ := http.NewRequest(http.MethodDelete, env.http.URL+"/api/v1/uploads/"+intent.UploadID, nil)
	request.Header.Set("X-Upload-Token", intent.UploadToken)
	response, err := env.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("abort multipart status = %d", response.StatusCode)
	}
	if env.fake.multipartIDFor(stagingKey(intent.UploadID)) != "" {
		t.Fatal("aborted multipart upload still exists")
	}
	status, _ = env.uploadStatus(t, intent)
	if status != http.StatusGone {
		t.Fatalf("aborted upload status = %d", status)
	}
	if parts, err := env.store.UploadParts(context.Background(), intent.UploadID); err != nil || len(parts) != 0 {
		t.Fatalf("upload parts after abort = %+v, %v", parts, err)
	}
}

func TestMultipartCompleteRateLimit(t *testing.T) {
	env := newTestEnv(t, 20<<20, 20)
	env.server.cfg.MultipartEnabled = true
	env.server.cfg.MultipartThreshold = 1
	env.server.cfg.MultipartPartSize = 3
	status, body, intent := env.createIntent(t, "", "rate-limited.bin", 7, "application/octet-stream")
	if status != http.StatusCreated {
		t.Fatalf("create multipart intent = %d: %s", status, body)
	}
	for i := 0; i < 20; i++ {
		code, _ := env.completeIntent(t, intent.UploadID, intent.UploadToken)
		if code != http.StatusConflict {
			t.Fatalf("completion attempt %d = %d", i, code)
		}
	}
	code, _ := env.completeIntent(t, intent.UploadID, intent.UploadToken)
	if code != http.StatusTooManyRequests {
		t.Fatalf("completion past budget = %d", code)
	}
}

func TestMultipartAbortFailureKeepsIntentPending(t *testing.T) {
	env := newTestEnv(t, 20<<20, 20)
	env.server.cfg.MultipartEnabled = true
	env.server.cfg.MultipartThreshold = 1
	env.server.cfg.MultipartPartSize = 3
	status, body, intent := env.createIntent(t, "", "abort-fails.bin", 7, "application/octet-stream")
	if status != http.StatusCreated {
		t.Fatalf("create multipart intent = %d: %s", status, body)
	}
	multipartID := env.fake.multipartIDFor(stagingKey(intent.UploadID))
	if multipartID == "" {
		t.Fatal("fake multipart upload was not created")
	}
	env.fake.abortErr = errors.New("provider abort unavailable")
	request, _ := http.NewRequest(http.MethodDelete, env.http.URL+"/api/v1/uploads/"+intent.UploadID, nil)
	request.Header.Set("X-Upload-Token", intent.UploadToken)
	response, err := env.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("failed abort status = %d", response.StatusCode)
	}
	stored, err := env.store.UploadIntentByID(context.Background(), intent.UploadID)
	if err != nil || stored.State != "pending" {
		t.Fatalf("intent state after failed abort = %+v, %v", stored, err)
	}
	if env.fake.multipartIDFor(stagingKey(intent.UploadID)) == "" {
		t.Fatal("provider multipart upload was lost after failed abort")
	}

	env.fake.abortErr = nil
	response, err = env.client.Do(request.Clone(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("retried abort status = %d", response.StatusCode)
	}
}

func TestMultipartRejectsOversizedProviderParts(t *testing.T) {
	env := newTestEnv(t, 20<<20, 20)
	env.server.cfg.MultipartEnabled = true
	env.server.cfg.MultipartThreshold = 1
	env.server.cfg.MultipartPartSize = 3
	status, body, intent := env.createIntent(t, "", "oversized-parts.bin", 6, "application/octet-stream")
	if status != http.StatusCreated {
		t.Fatalf("create multipart intent = %d: %s", status, body)
	}
	multipartID := env.fake.multipartIDFor(stagingKey(intent.UploadID))
	if multipartID == "" {
		t.Fatal("fake multipart upload was not created")
	}
	for partNumber := 1; partNumber <= 2; partNumber++ {
		etag := env.fake.putPart(multipartID, partNumber, make([]byte, 1000))
		if code := env.reportPart(t, intent, partNumber, etag, 3); code != http.StatusNoContent {
			t.Fatalf("report part %d status = %d", partNumber, code)
		}
	}
	completeStatus, completeBody := env.completeIntent(t, intent.UploadID, intent.UploadToken)
	if completeStatus != http.StatusRequestEntityTooLarge || !bytes.Contains(completeBody, []byte(`"part_size_mismatch"`)) {
		t.Fatalf("oversized provider part completion = %d: %s", completeStatus, completeBody)
	}
	if env.fake.multipartIDFor(stagingKey(intent.UploadID)) != "" {
		t.Fatal("oversized multipart upload was not aborted")
	}
}

func TestDeepSweepAbortsUnknownMultipart(t *testing.T) {
	env := newTestEnv(t, 20<<20, 20)
	id, err := env.fake.CreateMultipart(context.Background(), "files/uploads/orphan-multipart", "application/octet-stream", "")
	if err != nil {
		t.Fatal(err)
	}
	if env.fake.putPart(id, 1, []byte("orphan")) == "" {
		t.Fatal("failed to stage orphan part")
	}
	if _, err := env.server.deepSweep(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if env.fake.multipartIDFor("files/uploads/orphan-multipart") != "" {
		t.Fatal("deep sweep did not abort unknown multipart upload")
	}
}

func TestJanitorAbortsExpiredMultipart(t *testing.T) {
	env := newTestEnv(t, 20<<20, 20)
	env.server.cfg.MultipartEnabled = true
	env.server.cfg.MultipartThreshold = 1
	env.server.cfg.MultipartPartSize = 3
	status, body, intent := env.createIntent(t, "", "expired-multipart.bin", 7, "application/octet-stream")
	if status != http.StatusCreated {
		t.Fatalf("create multipart intent = %d: %s", status, body)
	}
	multipartID := env.fake.multipartIDFor(stagingKey(intent.UploadID))
	if multipartID == "" {
		t.Fatal("fake multipart upload was not created")
	}
	env.fake.putPart(multipartID, 1, []byte("abc"))
	env.server.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	env.server.MaintenanceOnce(context.Background())
	if env.fake.multipartIDFor(stagingKey(intent.UploadID)) != "" {
		t.Fatal("janitor did not abort expired multipart upload")
	}
	stored, err := env.store.UploadIntentByID(context.Background(), intent.UploadID)
	if err != nil || stored.State != "expired" {
		t.Fatalf("intent state = %+v, err = %v", stored, err)
	}
}

func TestGlobalFileCountCap(t *testing.T) {
	env := newTestEnv(t, 20<<20, 5<<20)
	env.server.cfg.MaxFiles = 1
	result := env.upload(t, "", "first.txt", []byte("x"))
	if result.Status != http.StatusCreated {
		t.Fatalf("first upload = %d: %s", result.Status, result.Body)
	}
	status, body, _ := env.createIntent(t, "", "second.txt", 1, "")
	if status != http.StatusInsufficientStorage {
		t.Fatalf("upload past file cap = %d: %s", status, body)
	}
}

func TestReservationPerClientCaps(t *testing.T) {
	env := newTestEnv(t, 20<<20, 5<<20)
	env.server.cfg.MaxPendingUploadsPerClient = 2
	env.server.cfg.MaxReservedBytesPerClient = 20 << 20
	for i := 0; i < 2; i++ {
		status, body, _ := env.createIntent(t, "", "pending.bin", 1, "")
		if status != http.StatusCreated {
			t.Fatalf("intent %d status = %d: %s", i, status, body)
		}
	}
	status, body, _ := env.createIntent(t, "", "third.bin", 1, "")
	if status != http.StatusInsufficientStorage {
		t.Fatalf("third intent status = %d: %s", status, body)
	}

	_, userHash := env.createKey(t)
	status, body, _ = env.createIntent(t, userHash, "owned.bin", 1, "")
	if status != http.StatusCreated {
		t.Fatalf("authenticated owner should have a separate reservation bucket: %d: %s", status, body)
	}

	bytesEnv := newTestEnv(t, 20<<20, 5<<20)
	bytesEnv.server.cfg.MaxPendingUploadsPerClient = 10
	bytesEnv.server.cfg.MaxReservedBytesPerClient = 2
	status, body, _ = bytesEnv.createIntent(t, "", "one.bin", 1, "")
	if status != http.StatusCreated {
		t.Fatalf("first byte-capped intent = %d: %s", status, body)
	}
	status, body, _ = bytesEnv.createIntent(t, "", "two.bin", 2, "")
	if status != http.StatusInsufficientStorage {
		t.Fatalf("second byte-capped intent = %d: %s", status, body)
	}
}

func TestClientIPTrustedProxyChain(t *testing.T) {
	s := &Server{cfg: config.Config{
		TrustProxy: true,
		TrustedProxyCIDRs: []netip.Prefix{
			netip.MustParsePrefix("127.0.0.1/32"),
			netip.MustParsePrefix("10.0.0.0/8"),
		},
	}}

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.2, 10.0.0.3")
	if got := s.clientIP(request); got != "203.0.113.9" {
		t.Fatalf("trusted chain IP = %q", got)
	}

	direct := httptest.NewRequest(http.MethodGet, "/", nil)
	direct.RemoteAddr = "198.51.100.7:1234"
	direct.Header.Set("X-Forwarded-For", "203.0.113.9")
	if got := s.clientIP(direct); got != "198.51.100.7" {
		t.Fatalf("untrusted peer spoofed IP = %q", got)
	}

	realIP := httptest.NewRequest(http.MethodGet, "/", nil)
	realIP.RemoteAddr = "127.0.0.1:1234"
	realIP.Header.Set("X-Real-IP", "203.0.113.10")
	if got := s.clientIP(realIP); got != "203.0.113.10" {
		t.Fatalf("trusted X-Real-IP = %q", got)
	}
}

func TestRateLimiterMapIsBounded(t *testing.T) {
	limiter := newRateLimiter()
	now := time.Now()
	limiter.mu.Lock()
	for i := 0; i < maxRateLimitWindows; i++ {
		limiter.windows[strconv.Itoa(i)] = &rateWindow{count: 1, reset: now.Add(time.Minute)}
	}
	limiter.mu.Unlock()
	limiter.allow("new-key", 1, time.Minute)
	limiter.mu.Lock()
	size := len(limiter.windows)
	limiter.mu.Unlock()
	if size > maxRateLimitWindows {
		t.Fatalf("limiter map size = %d", size)
	}
}

func TestHealthzCachesStorageProbe(t *testing.T) {
	env := newTestEnv(t, 20<<20, 5<<20)
	for i := 0; i < 2; i++ {
		status, body := getBody(t, env.client, env.http.URL+"/healthz")
		if status != http.StatusOK {
			t.Fatalf("health status = %d: %s", status, body)
		}
	}
	env.fake.mu.Lock()
	calls := env.fake.healthCalls
	env.fake.mu.Unlock()
	if calls != 1 {
		t.Fatalf("storage health calls = %d", calls)
	}
}

func TestSecureCookieNamesUseHostPrefix(t *testing.T) {
	secure := &Server{cfg: config.Config{CookieSecure: true}}
	if got := secure.cookieName(adminCookie); got != "__Host-interim_admin" {
		t.Fatalf("secure cookie name = %q", got)
	}
	local := &Server{cfg: config.Config{CookieSecure: false}}
	if got := local.cookieName(adminCookie); got != "interim_admin" {
		t.Fatalf("local cookie name = %q", got)
	}
}

func TestSanitizeStripsBidiControls(t *testing.T) {
	name := sanitizeFilename("invoice\u202Egnp.exe")
	if strings.ContainsRune(name, '\u202E') {
		t.Fatalf("bidi control survived sanitization: %q", name)
	}
	label := cleanLabel("label\u202Ename")
	if strings.ContainsRune(label, '\u202E') {
		t.Fatalf("bidi control survived label cleaning: %q", label)
	}
}

func TestRestrictedUploadsRequireAdminApprovedHash(t *testing.T) {
	env := newTestEnv(t, 20<<20, 5<<20)
	env.setupAdmin(t)
	unapprovedID, unapprovedHash := env.createKey(t)
	env.server.cfg.PublicUploadsEnabled = false

	status, home := getBody(t, env.client, env.http.URL+"/")
	if status != http.StatusOK {
		t.Fatalf("restricted home status = %d", status)
	}
	for _, needle := range []string{"Restricted uploads", `data-public-uploads="false"`, `id="upload-gate"`, "Enter the user hash"} {
		if !bytes.Contains([]byte(home), []byte(needle)) {
			t.Errorf("restricted home missing %q", needle)
		}
	}
	if bytes.Contains([]byte(home), []byte(`id="create-user-hash"`)) {
		t.Error("restricted home should not offer public user-hash creation")
	}

	status, statusBody := getBody(t, env.client, env.http.URL+"/api/v1/status")
	if status != http.StatusOK || !bytes.Contains([]byte(statusBody), []byte(`"public_uploads":false`)) {
		t.Fatalf("status = %d: %s", status, statusBody)
	}
	status, apiDocs := getBody(t, env.client, env.http.URL+"/api")
	if status != http.StatusOK || !bytes.Contains([]byte(apiDocs), []byte("Public user-hash creation is disabled")) {
		t.Fatalf("restricted API docs = %d", status)
	}

	response, err := env.client.Post(env.http.URL+"/api/v1/keys", "application/json", bytes.NewBufferString(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	keyBody, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden || !bytes.Contains(keyBody, []byte(`"key_creation_disabled"`)) {
		t.Fatalf("public key creation = %d: %s", response.StatusCode, keyBody)
	}

	status, intentBody, _ := env.createIntent(t, "", "anonymous.bin", 1, "")
	if status != http.StatusUnauthorized || !bytes.Contains(intentBody, []byte(`"uploads_restricted"`)) {
		t.Fatalf("anonymous restricted upload = %d: %s", status, intentBody)
	}
	status, intentBody, _ = env.createIntent(t, unapprovedHash, "unapproved.bin", 1, "")
	if status != http.StatusForbidden || !bytes.Contains(intentBody, []byte(`"upload_not_allowed"`)) {
		t.Fatalf("unapproved upload = %d: %s", status, intentBody)
	}

	status, adminBody := getBody(t, env.client, env.http.URL+"/admin")
	if status != http.StatusOK || !bytes.Contains([]byte(adminBody), []byte("Public uploads are disabled")) {
		t.Fatalf("admin status = %d", status)
	}
	csrf := extractCSRF(t, adminBody)
	response, err = env.client.PostForm(env.http.URL+"/admin/keys/create", url.Values{
		"csrf_token": {csrf},
		"name":       {"approved person"},
	})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("admin create key status = %d", response.StatusCode)
	}

	status, adminBody = getBody(t, env.client, env.http.URL+"/admin")
	if status != http.StatusOK {
		t.Fatalf("admin page after key creation = %d", status)
	}
	match := newKeyPattern.FindStringSubmatch(adminBody)
	if match == nil {
		t.Fatal("newly generated user hash was not shown once on the admin page")
	}
	adminHash := match[1]
	if !strings.HasPrefix(adminHash, "interim_") {
		t.Fatalf("generated hash = %q", adminHash)
	}
	if !bytes.Contains([]byte(adminBody), []byte(`action="/admin/keys/create"`)) {
		t.Error("admin page missing generate form")
	}

	status, intentBody, _ = env.createIntent(t, adminHash, "approved.bin", 1, "")
	if status != http.StatusCreated {
		t.Fatalf("admin-approved upload = %d: %s", status, intentBody)
	}

	request, _ := http.NewRequest(http.MethodGet, env.http.URL+"/api/v1/keys/me", nil)
	request.Header.Set("Authorization", "Bearer "+adminHash)
	keyResponse, err := env.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var info struct {
		Approved bool `json:"approved"`
	}
	if err := json.NewDecoder(keyResponse.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	keyResponse.Body.Close()
	if keyResponse.StatusCode != http.StatusOK || !info.Approved {
		t.Fatalf("admin key info = %d approved=%v", keyResponse.StatusCode, info.Approved)
	}

	status, repeatBody := getBody(t, env.client, env.http.URL+"/admin")
	if status != http.StatusOK {
		t.Fatalf("admin page reload = %d", status)
	}
	if newKeyPattern.MatchString(repeatBody) {
		t.Error("newly generated user hash was shown again after the one-time flash")
	}

	status, adminBody = getBody(t, env.client, env.http.URL+"/admin")
	if status != http.StatusOK {
		t.Fatalf("admin page before approval = %d", status)
	}
	csrf = extractCSRF(t, adminBody)
	response, err = env.client.PostForm(env.http.URL+"/admin/keys/update", url.Values{
		"csrf_token": {csrf},
		"id":         {unapprovedID},
		"action":     {"approve"},
	})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("approve status = %d", response.StatusCode)
	}
	status, intentBody, _ = env.createIntent(t, unapprovedHash, "now-approved.bin", 1, "")
	if status != http.StatusCreated {
		t.Fatalf("approved upload = %d: %s", status, intentBody)
	}

	status, adminBody = getBody(t, env.client, env.http.URL+"/admin")
	if status != http.StatusOK {
		t.Fatalf("admin page before unapproval = %d", status)
	}
	csrf = extractCSRF(t, adminBody)
	response, err = env.client.PostForm(env.http.URL+"/admin/keys/update", url.Values{
		"csrf_token": {csrf},
		"id":         {unapprovedID},
		"action":     {"unapprove"},
	})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	status, intentBody, _ = env.createIntent(t, unapprovedHash, "unapproved-again.bin", 1, "")
	if status != http.StatusForbidden || !bytes.Contains(intentBody, []byte(`"upload_not_allowed"`)) {
		t.Fatalf("unapproved upload after removal = %d: %s", status, intentBody)
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

	file, err := env.store.FileByID(context.Background(), result.ID)
	if err != nil {
		t.Fatal(err)
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
	if env.fake.exists(file.ObjectKey) {
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

	env.fake.putObject(stagingKey(intent.UploadID), []byte("hello world"), time.Now())
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
	env.fake.putObject(stagingKey(intent.UploadID), make([]byte, 20), time.Now())
	completeStatus, completeBody := env.completeIntent(t, intent.UploadID, intent.UploadToken)
	if completeStatus != http.StatusRequestEntityTooLarge {
		t.Fatalf("complete status = %d: %s", completeStatus, completeBody)
	}
	if env.fake.exists(stagingKey(intent.UploadID)) {
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
	env.fake.putObject(stagingKey(intent.UploadID), make([]byte, 10), time.Now())
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
	if env.fake.exists(stagingKey(intent.UploadID)) {
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
	env.fake.putObject(stagingKey(intent.UploadID), make([]byte, 10), time.Now())
	env.server.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	env.server.MaintenanceOnce(context.Background())
	if env.fake.exists(stagingKey(intent.UploadID)) {
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
	env.fake.putObject(stagingKey(active.UploadID), []byte("live"), time.Now())
	env.server.StartupMaintenance(context.Background())
	if env.fake.exists("files/orphan-object") {
		t.Fatal("startup sweep did not delete orphan object")
	}
	if !env.fake.exists(stagingKey(active.UploadID)) {
		t.Fatal("startup sweep deleted an active pending upload")
	}
}
