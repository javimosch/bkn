package files

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// The signature shape is checked here on every run; correctness against a real
// server is checked by TestS3RoundTripAgainstLiveServer below.
func TestS3RequestShape(t *testing.T) {
	var got *http.Request
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewS3(S3Config{
		Endpoint: srv.URL, Region: "eu-west-3", Bucket: "assets",
		AccessKey: "AKIDEXAMPLE", SecretKey: "secret", PathStyle: true,
	})
	if _, err := s.Put("ns/ab/abcdef", strings.NewReader("payload"), "text/plain"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if got.Method != http.MethodPut {
		t.Errorf("method = %s", got.Method)
	}
	if got.URL.Path != "/assets/ns/ab/abcdef" {
		t.Errorf("path = %q, want path-style with the bucket first", got.URL.Path)
	}
	if string(body) != "payload" {
		t.Errorf("body = %q", body)
	}

	auth := got.Header.Get("Authorization")
	for _, want := range []string{
		"AWS4-HMAC-SHA256 ",
		"Credential=AKIDEXAMPLE/",
		"/eu-west-3/s3/aws4_request",
		"SignedHeaders=host;x-amz-content-sha256;x-amz-date",
		"Signature=",
	} {
		if !strings.Contains(auth, want) {
			t.Errorf("Authorization %q is missing %q", auth, want)
		}
	}
	// The payload must be covered by the signature, not waved through with
	// UNSIGNED-PAYLOAD.
	if h := got.Header.Get("x-amz-content-sha256"); h != sha256Hex([]byte("payload")) {
		t.Errorf("x-amz-content-sha256 = %q, want the body digest", h)
	}
	if got.Header.Get("x-amz-date") == "" {
		t.Error("x-amz-date is missing")
	}
}

// Two different payloads must not produce the same signature.
func TestS3SignatureCoversTheRequest(t *testing.T) {
	s := NewS3(S3Config{Endpoint: "https://s3.example.com", Region: "us-east-1",
		Bucket: "b", AccessKey: "AK", SecretKey: "SK", PathStyle: true})
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	sigFor := func(method, key, payload string) string {
		u, _ := s.objectURL(key)
		req, _ := http.NewRequest(method, u, nil)
		s.sign(req, sha256Hex([]byte(payload)), at)
		return req.Header.Get("Authorization")
	}
	base := sigFor(http.MethodPut, "a/b/c", "one")
	for name, other := range map[string]string{
		"different payload": sigFor(http.MethodPut, "a/b/c", "two"),
		"different key":     sigFor(http.MethodPut, "a/b/d", "one"),
		"different method":  sigFor(http.MethodGet, "a/b/c", "one"),
	} {
		if other == base {
			t.Errorf("%s produced an identical signature", name)
		}
	}
	if sigFor(http.MethodPut, "a/b/c", "one") != base {
		t.Error("signing is not deterministic for a fixed timestamp")
	}
}

func TestS3EndpointStyles(t *testing.T) {
	virtual := NewS3(S3Config{Endpoint: "https://s3.eu-west-3.amazonaws.com",
		Region: "eu-west-3", Bucket: "assets", PathStyle: false})
	u, err := virtual.objectURL("ns/ab/cd")
	if err != nil || u != "https://assets.s3.eu-west-3.amazonaws.com/ns/ab/cd" {
		t.Errorf("virtual-host url = %q, %v", u, err)
	}
	pathStyle := NewS3(S3Config{Endpoint: "http://localhost:9000",
		Region: "us-east-1", Bucket: "assets", PathStyle: true})
	u, err = pathStyle.objectURL("ns/ab/cd")
	if err != nil || u != "http://localhost:9000/assets/ns/ab/cd" {
		t.Errorf("path-style url = %q, %v", u, err)
	}
}

// A custom endpoint implies a MinIO-style server, which addresses buckets by
// path rather than by subdomain.
func TestS3FromEnvDefaults(t *testing.T) {
	for _, name := range []string{"ENDPOINT", "REGION", "BUCKET", "ACCESS_KEY_ID", "SECRET_ACCESS_KEY", "FORCE_PATH_STYLE"} {
		t.Setenv("BKN_S3_"+name, "")
		t.Setenv("S3_"+name, "")
	}
	if S3FromEnv() != nil {
		t.Fatal("S3FromEnv built a backend with no credentials")
	}

	// The bare S3_* names are what the deployments being migrated already set.
	t.Setenv("S3_BUCKET", "legacy")
	t.Setenv("S3_ACCESS_KEY_ID", "AK")
	t.Setenv("S3_SECRET_ACCESS_KEY", "SK")
	s := S3FromEnv()
	if s == nil {
		t.Fatal("S3FromEnv ignored the S3_* fallback names")
	}
	if s.region != "us-east-1" || s.endpoint != "https://s3.us-east-1.amazonaws.com" || s.pathStyle {
		t.Errorf("defaults = region %q endpoint %q pathStyle %v", s.region, s.endpoint, s.pathStyle)
	}

	t.Setenv("S3_ENDPOINT", "http://localhost:9000")
	if s := S3FromEnv(); !s.pathStyle {
		t.Error("a custom endpoint should imply path-style addressing")
	}
}

// TestS3RoundTripAgainstLiveServer exercises the signer against a real
// S3-compatible server, which is the only way to know the signature is right.
//
//	docker run -d --rm -p 9123:9000 -e MINIO_ROOT_USER=minioadmin \
//	  -e MINIO_ROOT_PASSWORD=minioadmin quay.io/minio/minio server /data
//	BKN_S3_TEST_ENDPOINT=http://127.0.0.1:9123 go test ./internal/files/
func TestS3RoundTripAgainstLiveServer(t *testing.T) {
	endpoint := os.Getenv("BKN_S3_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("set BKN_S3_TEST_ENDPOINT to run the live S3 round trip")
	}
	access := envOr("BKN_S3_TEST_ACCESS_KEY", "minioadmin")
	secret := envOr("BKN_S3_TEST_SECRET_KEY", "minioadmin")
	bucket := envOr("BKN_S3_TEST_BUCKET", "bkntest")

	s := NewS3(S3Config{Endpoint: endpoint, Region: "us-east-1", Bucket: bucket,
		AccessKey: access, SecretKey: secret, PathStyle: true})

	// Create the bucket with the same signer; a wrong signature fails here.
	req, _ := http.NewRequest(http.MethodPut, strings.TrimSuffix(endpoint, "/")+"/"+bucket, nil)
	s.sign(req, sha256Hex(nil), time.Now())
	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusConflict {
		t.Fatalf("create bucket: HTTP %d", resp.StatusCode)
	}

	const content = "hello from bkn"
	loc, err := s.Put("ns/ab/abc123", strings.NewReader(content), "text/plain")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	rc, err := s.Get(loc)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != content {
		t.Fatalf("Get = %q, want %q", got, content)
	}
	if err := s.Delete(loc); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(loc); err != ErrNotFound {
		t.Fatalf("Get after Delete = %v, want ErrNotFound", err)
	}
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
