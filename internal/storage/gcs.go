package storage

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/compute/metadata"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const (
	gcsScope       = "https://www.googleapis.com/auth/cloud-platform"
	gcsDefaultHost = "https://storage.googleapis.com"
)

// GCS implements Storage using the Cloud Storage JSON API.
type GCS struct {
	bucket     string
	url        string
	client     *http.Client
	apiBase    string
	uploadBase string

	accessID   string
	privateKey []byte
	signBytes  func(context.Context, []byte) ([]byte, error)
}

// OpenGCS opens a Google Cloud Storage bucket from a gs:// URL.
func OpenGCS(ctx context.Context, urlStr string) (*GCS, error) {
	u, err := url.Parse(urlStr)
	if err != nil {
		return nil, fmt.Errorf("parsing GCS URL: %w", err)
	}
	if u.Scheme != "gs" || u.Host == "" {
		return nil, fmt.Errorf("invalid GCS URL %q", urlStr)
	}
	if u.Path != "" && u.Path != "/" {
		return nil, fmt.Errorf("GCS URL must name a bucket, got path %q", u.Path)
	}

	host := strings.TrimRight(gcsDefaultHost, "/")
	client := http.DefaultClient
	var accessID string
	var privateKey []byte

	if emulator := os.Getenv("STORAGE_EMULATOR_HOST"); emulator != "" {
		host = strings.TrimRight(emulator, "/")
		if !strings.HasPrefix(host, "http://") && !strings.HasPrefix(host, "https://") {
			host = "http://" + host
		}
	} else {
		var credsJSON []byte
		var err error
		client, credsJSON, err = gcsHTTPClient(ctx)
		if err != nil {
			return nil, err
		}
		accessID, privateKey = readGCSCredentials(credsJSON)
		if accessID == "" && metadata.OnGCE() {
			accessID, _ = metadata.Email("")
		}
	}

	g := &GCS{
		bucket:     u.Host,
		url:        urlStr,
		client:     client,
		apiBase:    host + "/storage/v1",
		uploadBase: host + "/upload/storage/v1",
		accessID:   accessID,
		privateKey: privateKey,
	}
	if len(privateKey) == 0 && accessID != "" {
		g.signBytes = g.signBlob
	}
	return g, nil
}

func gcsHTTPClient(ctx context.Context) (*http.Client, []byte, error) {
	creds, err := google.FindDefaultCredentials(ctx, gcsScope)
	if err != nil {
		return nil, nil, fmt.Errorf("loading GCS default credentials: %w", err)
	}
	return oauth2.NewClient(ctx, creds.TokenSource), creds.JSON, nil
}

func readGCSCredentials(credFileAsJSON []byte) (string, []byte) {
	var serviceAccount struct {
		ClientEmail string `json:"client_email"`
		PrivateKey  string `json:"private_key"`
	}
	if err := json.Unmarshal(credFileAsJSON, &serviceAccount); err == nil && serviceAccount.ClientEmail != "" {
		return serviceAccount.ClientEmail, []byte(serviceAccount.PrivateKey)
	}

	var impersonated struct {
		ServiceAccountImpersonationURL string `json:"service_account_impersonation_url"`
	}
	if err := json.Unmarshal(credFileAsJSON, &impersonated); err == nil {
		if email := serviceAccountFromImpersonationURL(impersonated.ServiceAccountImpersonationURL); email != "" {
			return email, nil
		}
	}

	return "", nil
}

func serviceAccountFromImpersonationURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	const marker = "/serviceAccounts/"
	idx := strings.Index(u.Path, marker)
	if idx == -1 {
		return ""
	}
	email := strings.TrimSuffix(u.Path[idx+len(marker):], ":generateAccessToken")
	email, _ = url.PathUnescape(email)
	return email
}

func (g *GCS) Store(ctx context.Context, path string, r io.Reader) (int64, string, error) {
	h := sha256.New()
	body := &countingReader{r: io.TeeReader(r, h)}

	endpoint := g.uploadBase + "/b/" + url.PathEscape(g.bucket) + "/o"
	reqURL, _ := url.Parse(endpoint)
	q := reqURL.Query()
	q.Set("uploadType", "media")
	q.Set("name", path)
	reqURL.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL.String(), body)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := g.client.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("uploading GCS object: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := g.checkResponse(resp, http.StatusOK); err != nil {
		return 0, "", fmt.Errorf("uploading GCS object: %w", err)
	}

	hash := hex.EncodeToString(h.Sum(nil))
	return body.n, hash, nil
}

func (g *GCS) Open(ctx context.Context, path string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.objectURL(path)+"?alt=media", nil)
	if err != nil {
		return nil, err
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("opening GCS object: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		return nil, ErrNotFound
	}
	if err := g.checkResponse(resp, http.StatusOK); err != nil {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("opening GCS object: %w", err)
	}
	return resp.Body, nil
}

func (g *GCS) Exists(ctx context.Context, path string) (bool, error) {
	_, err := g.attrs(ctx, path)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (g *GCS) Delete(ctx context.Context, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, g.objectURL(path), nil)
	if err != nil {
		return err
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return fmt.Errorf("deleting GCS object: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if err := g.checkResponse(resp, http.StatusNoContent); err != nil {
		return fmt.Errorf("deleting GCS object: %w", err)
	}
	return nil
}

func (g *GCS) Size(ctx context.Context, path string) (int64, error) {
	obj, err := g.attrs(ctx, path)
	if err != nil {
		return 0, err
	}
	return obj.size(), nil
}

func (g *GCS) SignedURL(ctx context.Context, path string, expiry time.Duration) (string, error) {
	if g.accessID == "" {
		return "", ErrSignedURLUnsupported
	}

	expires := time.Now().Add(expiry)
	u := &url.URL{Path: fmt.Sprintf("/%s/%s", g.bucket, path)}
	stringToSign := fmt.Sprintf("GET\n\n\n%d\n%s", expires.Unix(), u.String())

	signed, err := g.sign(ctx, []byte(stringToSign))
	if err != nil {
		return "", fmt.Errorf("signing GCS URL: %w", err)
	}

	u.Scheme = "https"
	u.Host = "storage.googleapis.com"
	q := u.Query()
	q.Set("GoogleAccessId", g.accessID)
	q.Set("Expires", strconv.FormatInt(expires.Unix(), 10))
	q.Set("Signature", base64.StdEncoding.EncodeToString(signed))
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (g *GCS) UsedSpace(ctx context.Context) (int64, error) {
	objects, err := g.ListPrefix(ctx, "")
	if err != nil {
		return 0, err
	}
	var total int64
	for _, obj := range objects {
		total += obj.Size
	}
	return total, nil
}

func (g *GCS) ListPrefix(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	var objects []ObjectInfo
	pageToken := ""

	for {
		reqURL, _ := url.Parse(g.apiBase + "/b/" + url.PathEscape(g.bucket) + "/o")
		q := reqURL.Query()
		q.Set("prefix", prefix)
		if pageToken != "" {
			q.Set("pageToken", pageToken)
		}
		reqURL.RawQuery = q.Encode()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL.String(), nil)
		if err != nil {
			return nil, err
		}
		resp, err := g.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("listing GCS objects: %w", err)
		}
		if err := g.checkResponse(resp, http.StatusOK); err != nil {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("listing GCS objects: %w", err)
		}

		var page gcsListResponse
		if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("decoding GCS list response: %w", err)
		}
		_ = resp.Body.Close()

		for _, item := range page.Items {
			objects = append(objects, ObjectInfo{
				Path:    item.Name,
				Size:    item.size(),
				ModTime: item.updated(),
			})
		}
		if page.NextPageToken == "" {
			return objects, nil
		}
		pageToken = page.NextPageToken
	}
}

func (g *GCS) Close() error {
	return nil
}

func (g *GCS) URL() string {
	return g.url
}

func (g *GCS) attrs(ctx context.Context, path string) (*gcsObject, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.objectURL(path), nil)
	if err != nil {
		return nil, err
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("getting GCS object attributes: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if err := g.checkResponse(resp, http.StatusOK); err != nil {
		return nil, fmt.Errorf("getting GCS object attributes: %w", err)
	}

	var obj gcsObject
	if err := json.NewDecoder(resp.Body).Decode(&obj); err != nil {
		return nil, fmt.Errorf("decoding GCS attributes: %w", err)
	}
	return &obj, nil
}

func (g *GCS) objectURL(path string) string {
	return g.apiBase + "/b/" + url.PathEscape(g.bucket) + "/o/" + url.PathEscape(path)
}

func (g *GCS) checkResponse(resp *http.Response, want int) error {
	if resp.StatusCode == want {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
}

func (g *GCS) sign(ctx context.Context, b []byte) ([]byte, error) {
	if len(g.privateKey) > 0 {
		key, err := parseGCSPrivateKey(g.privateKey)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(b)
		return rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	}
	if g.signBytes != nil {
		return g.signBytes(ctx, b)
	}
	return nil, ErrSignedURLUnsupported
}

func (g *GCS) signBlob(ctx context.Context, payload []byte) ([]byte, error) {
	reqBody, err := json.Marshal(map[string]string{
		"payload": base64.StdEncoding.EncodeToString(payload),
	})
	if err != nil {
		return nil, err
	}

	endpoint := "https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/" +
		url.PathEscape(g.accessID) + ":signBlob"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling IAM signBlob: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := g.checkResponse(resp, http.StatusOK); err != nil {
		return nil, fmt.Errorf("calling IAM signBlob: %w", err)
	}

	var out struct {
		SignedBlob string `json:"signedBlob"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decoding IAM signBlob response: %w", err)
	}
	return base64.StdEncoding.DecodeString(out.SignedBlob)
}

func parseGCSPrivateKey(key []byte) (*rsa.PrivateKey, error) {
	if block, _ := pem.Decode(key); block != nil {
		key = block.Bytes
	}
	parsedKey, err := x509.ParsePKCS8PrivateKey(key)
	if err != nil {
		parsedKey, err = x509.ParsePKCS1PrivateKey(key)
		if err != nil {
			return nil, err
		}
	}
	parsed, ok := parsedKey.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("private key is not RSA")
	}
	return parsed, nil
}

type gcsObject struct {
	Name    string `json:"name"`
	Size    string `json:"size"`
	Updated string `json:"updated"`
}

type countingReader struct {
	r io.Reader
	n int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.n += int64(n)
	return n, err
}

func (o gcsObject) size() int64 {
	n, _ := strconv.ParseInt(o.Size, 10, 64)
	return n
}

func (o gcsObject) updated() time.Time {
	t, _ := time.Parse(time.RFC3339Nano, o.Updated)
	return t
}

type gcsListResponse struct {
	NextPageToken string      `json:"nextPageToken"`
	Items         []gcsObject `json:"items"`
}
