package main

import (
	"bytes"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testServerWithLogging(t *testing.T, logger *log.Logger, browserLogging bool) (*server, string) {
	t.Helper()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	srv, err := newServer(root, logger, browserLogging)
	if err != nil {
		t.Fatal(err)
	}
	return srv, dir
}

func testServer(t *testing.T) (*server, string) {
	t.Helper()
	return testServerWithLogging(t, log.New(io.Discard, ``, 0), false)
}

func uploadRequest(t *testing.T, target, name string, data []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile(`file`, name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, target, &body)
	req.Header.Set(`Content-Type`, mw.FormDataContentType())
	return req
}

func TestUploadAllowsGitDirectoryAndReplacesFile(t *testing.T) {
	srv, dir := testServer(t)
	gitDir := filepath.Join(dir, `.git`)
	if err := os.Mkdir(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, content := range []string{`first`, `second`} {
		rec := httptest.NewRecorder()
		srv.upload(rec, uploadRequest(t, `/upload?dir=.git`, `config`, []byte(content)))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("upload status = %d, body %q", rec.Code, rec.Body.String())
		}
		got, err := os.ReadFile(filepath.Join(gitDir, `config`))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != content {
			t.Fatalf("uploaded content = %q, want %q", got, content)
		}
	}
}

func TestSymlinkDirectoryIsNotFollowed(t *testing.T) {
	srv, dir := testServer(t)
	realDir := filepath.Join(dir, `real`)
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realDir, `inside.txt`), []byte(`inside`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(`real`, filepath.Join(dir, `linked-dir`)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(`real`, `inside.txt`), filepath.Join(dir, `linked-file.txt`)); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	srv.page(rec, httptest.NewRequest(http.MethodGet, `/linked-dir/`, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("symlink directory status = %d, want 404", rec.Code)
	}

	rec = httptest.NewRecorder()
	srv.upload(rec, uploadRequest(t, `/upload?dir=linked-dir`, `new.txt`, []byte(`new`)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("symlink directory upload status = %d, want 404", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(realDir, `new.txt`)); !os.IsNotExist(err) {
		t.Fatalf("upload followed symlink directory: %v", err)
	}

	rec = httptest.NewRecorder()
	srv.qr(rec, httptest.NewRequest(http.MethodGet, `/qr/linked-dir/inside.txt`, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("symlink directory QR status = %d, want 404", rec.Code)
	}

	rec = httptest.NewRecorder()
	srv.page(rec, httptest.NewRequest(http.MethodGet, `/linked-file.txt`, nil))
	if rec.Code != http.StatusOK || rec.Body.String() != `inside` {
		t.Fatalf("symlink file response = %d %q", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	srv.page(rec, httptest.NewRequest(http.MethodGet, `/`, nil))
	if !strings.Contains(rec.Body.String(), `symlink directory not served`) {
		t.Fatalf("directory listing does not explain skipped symlink: %q", rec.Body.String())
	}
}

func TestBadUploadDoesNotBreakLaterRequests(t *testing.T) {
	srv, _ := testServer(t)
	rec := httptest.NewRecorder()
	srv.upload(rec, uploadRequest(t, `/upload?dir=missing`, `file.txt`, []byte(`data`)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("bad upload status = %d, want 404", rec.Code)
	}

	rec = httptest.NewRecorder()
	srv.page(rec, httptest.NewRequest(http.MethodGet, `/`, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("request after bad upload = %d, want 200", rec.Code)
	}
}

func TestDirectoryListingEscapesFileURLs(t *testing.T) {
	srv, dir := testServer(t)
	name := `what?# %.txt`
	if err := os.WriteFile(filepath.Join(dir, name), []byte(`content`), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	srv.page(rec, httptest.NewRequest(http.MethodGet, `/`, nil))
	want := `href="` + url.PathEscape(name) + `"`
	if !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("listing does not contain %q: %q", want, rec.Body.String())
	}
}

func TestHandlerMethodsAndOrigins(t *testing.T) {
	srv, _ := testServer(t)
	rec := httptest.NewRecorder()
	srv.page(rec, httptest.NewRequest(http.MethodPost, `/`, nil))
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get(`Allow`) != `GET, HEAD` {
		t.Fatalf("wrong-method response = %d Allow=%q", rec.Code, rec.Header().Get(`Allow`))
	}

	req := uploadRequest(t, `/upload`, `file.txt`, []byte(`data`))
	req.Host = `host.example:8000`
	req.Header.Set(`Origin`, `https://host.example:8000`)
	rec = httptest.NewRecorder()
	srv.upload(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin upload status = %d, want 403", rec.Code)
	}
}

func TestBrowserLoggingDisabled(t *testing.T) {
	srv, _ := testServer(t)
	handler := srv.mux()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, `/log`, strings.NewReader(`message`)))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("disabled log endpoint status = %d, want 405", rec.Code)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, `/`, nil))
	body := rec.Body.String()
	if strings.Contains(body, `fetch('/log'`) || strings.Contains(body, `log=1`) {
		t.Fatalf("disabled page contains browser logging code: %q", body)
	}
}

func TestBrowserLoggingEnabledAndDefensive(t *testing.T) {
	var logs bytes.Buffer
	srv, _ := testServerWithLogging(t, log.New(&logs, ``, 0), true)
	handler := srv.mux()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, `/`, nil))
	body := rec.Body.String()
	if !strings.Contains(body, `fetch('/log'`) || !strings.Contains(body, `log=1`) {
		t.Fatalf("enabled page omits browser logging code: %q", body)
	}

	req := httptest.NewRequest(http.MethodPost, `http://serve.test/log`, strings.NewReader("scanner ready\nforged line"))
	req.Header.Set(`Origin`, `http://serve.test`)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("log status = %d, body %q", rec.Code, rec.Body.String())
	}
	if got := logs.String(); !strings.Contains(got, `browser message="scanner ready\nforged line"`) || strings.Contains(got, "\nforged line") {
		t.Fatalf("browser log was not safely quoted: %q", got)
	}

	req = httptest.NewRequest(http.MethodPost, `http://serve.test/log`, strings.NewReader(`cross origin`))
	req.Header.Set(`Origin`, `https://other.test`)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin log status = %d, want 403", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, `http://serve.test/log`, strings.NewReader(strings.Repeat(`x`, maxClientLog+1)))
	req.Header.Set(`Origin`, `http://serve.test`)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized log status = %d, want 413", rec.Code)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, `/log`, nil))
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get(`Allow`) != http.MethodPost {
		t.Fatalf("wrong-method log response = %d Allow=%q", rec.Code, rec.Header().Get(`Allow`))
	}
}

func TestQRRejectsOversizedFile(t *testing.T) {
	srv, dir := testServer(t)
	f, err := os.Create(filepath.Join(dir, `large.bin`))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxQRFileSize + 1); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	srv.qr(rec, httptest.NewRequest(http.MethodGet, `/qr/large.bin`, nil))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized QR status = %d, want 413", rec.Code)
	}
}
