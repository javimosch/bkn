package files

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"

	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// S3 is an S3-compatible backend speaking the REST API directly.
//
// No SDK. Object storage needs three verbs and one signing algorithm, and the
// official client pulls in a dependency tree larger than this entire program.
// It works against AWS S3 and against anything that speaks the same dialect,
// MinIO included.
type S3 struct {
	endpoint  string // https://s3.eu-west-3.amazonaws.com or http://localhost:9000
	region    string
	bucket    string
	accessKey string
	secretKey string
	pathStyle bool
	client    *http.Client
}

// S3Config configures the backend.
type S3Config struct {
	Endpoint  string
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
	PathStyle bool
}

// S3FromEnv builds an S3 backend from the environment, or returns nil when it
// is not configured. BKN_S3_* wins; the bare S3_* names are read as a fallback
// because that is what the deployments being migrated already set.
func S3FromEnv() *S3 {
	get := func(name string) string {
		if v := os.Getenv("BKN_S3_" + name); v != "" {
			return v
		}
		return os.Getenv("S3_" + name)
	}
	cfg := S3Config{
		Endpoint:  get("ENDPOINT"),
		Region:    get("REGION"),
		Bucket:    get("BUCKET"),
		AccessKey: get("ACCESS_KEY_ID"),
		SecretKey: get("SECRET_ACCESS_KEY"),
		PathStyle: strings.EqualFold(get("FORCE_PATH_STYLE"), "true") || get("FORCE_PATH_STYLE") == "1",
	}
	if cfg.Bucket == "" || cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = "https://s3." + cfg.Region + ".amazonaws.com"
	} else {
		// A custom endpoint is almost always MinIO or a compatible service,
		// which addresses buckets by path rather than by subdomain.
		cfg.PathStyle = true
	}
	return NewS3(cfg)
}

func NewS3(cfg S3Config) *S3 {
	return &S3{
		endpoint:  strings.TrimSuffix(cfg.Endpoint, "/"),
		region:    cfg.Region,
		bucket:    cfg.Bucket,
		accessKey: cfg.AccessKey,
		secretKey: cfg.SecretKey,
		pathStyle: cfg.PathStyle,
		client:    &http.Client{Timeout: 60 * time.Second},
	}
}

func (s *S3) Name() string { return BackendS3 }

// objectURL builds the request URL for a key.
func (s *S3) objectURL(key string) (string, error) {
	u, err := url.Parse(s.endpoint)
	if err != nil {
		return "", err
	}
	if s.pathStyle {
		u.Path = "/" + s.bucket + "/" + key
	} else {
		u.Host = s.bucket + "." + u.Host
		u.Path = "/" + key
	}
	return u.String(), nil
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// encodePath URI-encodes each path segment, leaving the separators alone,
// which is what the canonical request expects.
func encodePath(p string) string {
	parts := strings.Split(p, "/")
	for i, seg := range parts {
		parts[i] = strings.ReplaceAll(url.QueryEscape(seg), "+", "%20")
	}
	return strings.Join(parts, "/")
}

// sign applies AWS Signature Version 4 to a request whose body is already
// hashed. The payload hash is always explicit: S3 requires the
// x-amz-content-sha256 header, and UNSIGNED-PAYLOAD is deliberately not used
// so the bytes are covered by the signature.
func (s *S3) sign(req *http.Request, payloadHash string, at time.Time) {
	amzDate := at.UTC().Format("20060102T150405Z")
	dateStamp := at.UTC().Format("20060102")
	scope := dateStamp + "/" + s.region + "/s3/aws4_request"

	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	req.Header.Set("host", req.URL.Host)

	// Only the three headers below are signed. Keeping the set fixed and
	// small avoids the classic failure where a proxy adds a header and
	// invalidates a signature computed over "whatever was present".
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalHeaders := "host:" + req.URL.Host + "\n" +
		"x-amz-content-sha256:" + payloadHash + "\n" +
		"x-amz-date:" + amzDate + "\n"

	canonicalRequest := strings.Join([]string{
		req.Method,
		encodePath(req.URL.Path),
		req.URL.RawQuery,
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	key := hmacSHA256([]byte("AWS4"+s.secretKey), dateStamp)
	key = hmacSHA256(key, s.region)
	key = hmacSHA256(key, "s3")
	key = hmacSHA256(key, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(key, stringToSign))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		s.accessKey, scope, signedHeaders, signature))
}

func (s *S3) do(method, key string, body []byte, contentType string) (*http.Response, error) {
	target, err := s.objectURL(key)
	if err != nil {
		return nil, err
	}
	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(string(body))
	}
	req, err := http.NewRequest(method, target, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = int64(len(body))
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	s.sign(req, sha256Hex(body), time.Now())
	return s.client.Do(req)
}

func (s *S3) Put(key string, r io.Reader, contentType string) (string, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	resp, err := s.do(http.MethodPut, key, body, contentType)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", s3Error("PUT", key, resp)
	}
	return key, nil
}

func (s *S3) Get(location string) (io.ReadCloser, error) {
	resp, err := s.do(http.MethodGet, location, nil, "")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, ErrNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		return nil, s3Error("GET", location, resp)
	}
	return resp.Body, nil
}

func (s *S3) Delete(location string) error {
	resp, err := s.do(http.MethodDelete, location, nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// S3 reports deleting a missing object as success, which matches the
	// local backend's behaviour.
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return s3Error("DELETE", location, resp)
	}
	return nil
}

func s3Error(op, key string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = resp.Status
	}
	return fmt.Errorf("s3 %s %s: %s", op, key, msg)
}
