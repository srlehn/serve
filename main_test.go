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
	return uploadFilesRequest(t, target, uploadFile{name: name, data: data})
}

type uploadFile struct {
	name string
	data []byte
}

func uploadBody(t *testing.T, files ...uploadFile) ([]byte, string) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, file := range files {
		part, err := mw.CreateFormFile(`file`, file.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(file.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes(), mw.FormDataContentType()
}

func uploadFilesRequest(t *testing.T, target string, files ...uploadFile) *http.Request {
	t.Helper()
	body, contentType := uploadBody(t, files...)
	req := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	req.Header.Set(`Content-Type`, contentType)
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

func TestInterruptedUploadIsARequestErrorAndAtomic(t *testing.T) {
	var logs bytes.Buffer
	srv, dir := testServerWithLogging(t, log.New(&logs, ``, 0), false)
	target := filepath.Join(dir, `photo.png`)
	if err := os.WriteFile(target, []byte(`original`), 0o644); err != nil {
		t.Fatal(err)
	}

	data := bytes.Repeat([]byte(`x`), 4096)
	body, contentType := uploadBody(t, uploadFile{name: `photo.png`, data: data})
	body = body[:len(body)-len(data)/2]
	req := httptest.NewRequest(http.MethodPost, `/upload`, bytes.NewReader(body))
	req.Header.Set(`Content-Type`, contentType)
	rec := httptest.NewRecorder()
	srv.upload(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("interrupted upload status = %d, body %q", rec.Code, rec.Body.String())
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != `original` {
		t.Fatalf("destination after interrupted upload = %q, %v", got, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), `.serve-upload-`) {
			t.Fatalf("interrupted upload left temporary file %q", entry.Name())
		}
	}
	if got := logs.String(); !strings.Contains(got, `status=400 error="unexpected EOF"`) || strings.Contains(got, `status=500`) {
		t.Fatalf("interrupted upload log = %q", got)
	}

	rec = httptest.NewRecorder()
	srv.upload(rec, uploadRequest(t, `/upload`, `photo.png`, []byte(`complete`)))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("following upload status = %d, body %q", rec.Code, rec.Body.String())
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != `complete` {
		t.Fatalf("destination after complete upload = %q, %v", got, err)
	}
	if got := logs.String(); !strings.Contains(got, `upload path="photo.png" bytes=8`) {
		t.Fatalf("successful upload log = %q", got)
	}
}

func TestUploadStoresMultipleFiles(t *testing.T) {
	srv, dir := testServer(t)
	rec := httptest.NewRecorder()
	srv.upload(rec, uploadFilesRequest(t, `/upload`,
		uploadFile{name: `one.txt`, data: []byte(`one`)},
		uploadFile{name: `two.txt`, data: []byte(`second`)},
	))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("multi-file upload status = %d, body %q", rec.Code, rec.Body.String())
	}
	for name, want := range map[string]string{`one.txt`: `one`, `two.txt`: `second`} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(got) != want {
			t.Errorf("uploaded %s = %q, %v; want %q", name, got, err, want)
		}
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

func TestRawFilesUseSafeContentTypes(t *testing.T) {
	srv, dir := testServer(t)
	tests := []struct {
		name        string
		content     []byte
		contentType string
	}{
		{`page.html`, []byte(`<!doctype html><h1>active</h1>`), `text/plain; charset=utf-8`},
		{`page.html.template`, []byte(`<h1>{{.Value}}</h1>`), `text/plain; charset=utf-8`},
		{`script.js`, []byte(`alert('active')`), `text/plain; charset=utf-8`},
		{`drawing.svg`, []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`), `text/plain; charset=utf-8`},
		{`unknown.data`, []byte(`<h1>sniffed markup</h1>`), `text/plain; charset=utf-8`},
		{`photo.png`, []byte("\x89PNG\r\n\x1a\n"), `image/png`},
		{`manual.pdf`, []byte("%PDF-1.7\n"), `application/pdf`},
		{`blob.bin`, []byte{0, 1, 2, 3}, `application/octet-stream`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(dir, tt.name), tt.content, 0o644); err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			srv.page(rec, httptest.NewRequest(http.MethodGet, `/`+tt.name, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("raw file status = %d", rec.Code)
			}
			if got := rec.Header().Get(`Content-Type`); got != tt.contentType {
				t.Fatalf("raw file content type = %q, want %q", got, tt.contentType)
			}
			if got := rec.Header().Get(`X-Content-Type-Options`); got != `nosniff` {
				t.Fatalf("X-Content-Type-Options = %q", got)
			}
			if !bytes.Equal(rec.Body.Bytes(), tt.content) {
				t.Fatalf("raw file body = %q", rec.Body.Bytes())
			}
		})
	}

	rec := httptest.NewRecorder()
	srv.page(rec, httptest.NewRequest(http.MethodGet, `/`, nil))
	if got := rec.Header().Get(`Content-Type`); got != `text/html; charset=utf-8` {
		t.Fatalf("generated page content type = %q", got)
	}
}

func TestUploadPageUsesControlledSubmission(t *testing.T) {
	srv, _ := testServer(t)
	rec := httptest.NewRecorder()
	srv.page(rec, httptest.NewRequest(http.MethodGet, `/`, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("page status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`id="uploadForm"`,
		`id="uploadStatus"`,
		`uploadForm.addEventListener('submit'`,
		`request.upload.addEventListener('progress'`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page does not contain %q", want)
		}
	}
	for _, unwanted := range []string{`target="dummyFrame"`, `<iframe`} {
		if strings.Contains(body, unwanted) {
			t.Errorf("page still contains %q", unwanted)
		}
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
