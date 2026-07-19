package main

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/srlehn/serve/jabstream"
)

func testServerWithOptions(t *testing.T, logger *log.Logger, browserLogging bool, uploadLimit int64) (*server, string) {
	t.Helper()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	srv, err := newServer(root.FS(), root, logger, browserLogging, uploadLimit)
	if err != nil {
		t.Fatal(err)
	}
	return srv, dir
}

func testServerWithLogging(t *testing.T, logger *log.Logger, browserLogging bool) (*server, string) {
	t.Helper()
	return testServerWithOptions(t, logger, browserLogging, 0)
}

func testServerWithUploadLimit(t *testing.T, uploadLimit int64) (*server, string) {
	t.Helper()
	return testServerWithOptions(t, log.New(io.Discard, ``, 0), false, uploadLimit)
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

func TestUploadLimitZeroIsUnlimited(t *testing.T) {
	srv, dir := testServerWithUploadLimit(t, 0)
	data := bytes.Repeat([]byte(`x`), 1<<20)
	rec := httptest.NewRecorder()
	srv.upload(rec, uploadRequest(t, `/upload`, `large.dat`, data))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unlimited upload status = %d, body %q", rec.Code, rec.Body.String())
	}
	if info, err := os.Stat(filepath.Join(dir, `large.dat`)); err != nil || info.Size() != int64(len(data)) {
		t.Fatalf("unlimited upload size = %v, %v; want %d", info, err, len(data))
	}
}

func TestParseByteSize(t *testing.T) {
	tests := []struct {
		value string
		want  int64
	}{
		{`0`, 0},
		{`0 MB`, 0},
		{`1B`, 1},
		{`1KB`, 1000},
		{`500MB`, 500_000_000},
		{`1.5GB`, 1_500_000_000},
		{`1KiB`, 1 << 10},
		{`512MiB`, 512 << 20},
		{`2GiB`, 2 << 30},
		{` 2 gib `, 2 << 30},
		{`1TiB`, 1 << 40},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			got, err := parseByteSize(tt.value)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("parseByteSize(%q) = %d, want %d", tt.value, got, tt.want)
			}
		})
	}
}

func TestParseByteSizeRejectsInvalidValues(t *testing.T) {
	for _, value := range []string{``, `1`, `-1GB`, `MB`, `1XB`, `1.1B`, `100000000000000000000GiB`} {
		t.Run(value, func(t *testing.T) {
			if _, err := parseByteSize(value); err == nil {
				t.Fatalf("parseByteSize(%q) succeeded", value)
			}
		})
	}
}

func TestUploadLimitAcceptsExactRequestSize(t *testing.T) {
	body, contentType := uploadBody(t, uploadFile{name: `exact.txt`, data: []byte(`content`)})
	srv, dir := testServerWithUploadLimit(t, int64(len(body)))
	req := httptest.NewRequest(http.MethodPost, `/upload`, bytes.NewReader(body))
	req.Header.Set(`Content-Type`, contentType)
	rec := httptest.NewRecorder()
	srv.upload(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("exact-limit upload status = %d, body %q", rec.Code, rec.Body.String())
	}
	if got, err := os.ReadFile(filepath.Join(dir, `exact.txt`)); err != nil || string(got) != `content` {
		t.Fatalf("exact-limit upload = %q, %v", got, err)
	}
}

func TestUploadLimitIncludesMultipartOverhead(t *testing.T) {
	data := bytes.Repeat([]byte(`x`), 128)
	body, contentType := uploadBody(t, uploadFile{name: `overhead.dat`, data: data})
	srv, dir := testServerWithUploadLimit(t, int64(len(data)))
	req := httptest.NewRequest(http.MethodPost, `/upload`, bytes.NewReader(body))
	req.Header.Set(`Content-Type`, contentType)
	rec := httptest.NewRecorder()
	srv.upload(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("multipart-overhead status = %d, body %q", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, `overhead.dat`)); !os.IsNotExist(err) {
		t.Fatalf("multipart-overhead upload created destination: %v", err)
	}
}

func TestStreamingUploadLimitIsAtomicAndServerRecovers(t *testing.T) {
	data := bytes.Repeat([]byte(`x`), 4096)
	body, contentType := uploadBody(t, uploadFile{name: `photo.png`, data: data})
	limit := int64(len(body) - len(data)/2)
	srv, dir := testServerWithUploadLimit(t, limit)
	target := filepath.Join(dir, `photo.png`)
	if err := os.WriteFile(target, []byte(`original`), 0o644); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, `/upload`, bytes.NewReader(body))
	req.ContentLength = -1
	req.Header.Set(`Content-Type`, contentType)
	rec := httptest.NewRecorder()
	srv.upload(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("streaming oversized upload status = %d, body %q", rec.Code, rec.Body.String())
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != `original` {
		t.Fatalf("destination after oversized upload = %q, %v", got, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), `.serve-upload-`) {
			t.Fatalf("oversized upload left temporary file %q", entry.Name())
		}
	}

	rec = httptest.NewRecorder()
	srv.upload(rec, uploadRequest(t, `/upload`, `photo.png`, []byte(`complete`)))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("following upload status = %d, body %q", rec.Code, rec.Body.String())
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != `complete` {
		t.Fatalf("destination after following upload = %q, %v", got, err)
	}
}

func TestNewServerRejectsNegativeUploadLimit(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, err := newServer(root.FS(), root, log.New(io.Discard, ``, 0), false, -1); err == nil {
		t.Fatal("newServer accepted a negative upload limit")
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

func TestServerReadSideUsesProvidedFS(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, `root-only.txt`), []byte(`wrong tree`), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	srv, err := newServer(
		fstest.MapFS{`virtual.txt`: {Data: []byte(`virtual tree`)}},
		root,
		log.New(io.Discard, ``, 0),
		false,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if srv.archiveRoot != nil {
		t.Fatal("writable root was incorrectly associated with a different read filesystem")
	}

	rec := httptest.NewRecorder()
	srv.page(rec, httptest.NewRequest(http.MethodGet, `/virtual.txt`, nil))
	if rec.Code != http.StatusOK || rec.Body.String() != `virtual tree` {
		t.Fatalf("virtual file response = %d %q", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	srv.page(rec, httptest.NewRequest(http.MethodGet, `/root-only.txt`, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("writable-root-only file status = %d, want 404", rec.Code)
	}
}

func TestServerWithoutUploadRootIsReadOnly(t *testing.T) {
	srv, err := newServer(
		fstest.MapFS{`upload`: {Data: []byte(`ordinary file`)}},
		nil,
		log.New(io.Discard, ``, 0),
		false,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	handler := srv.mux()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, `/`, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("read-only listing status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, unwanted := range []string{`id="uploadForm"`, `uploadForm.addEventListener`} {
		if strings.Contains(body, unwanted) {
			t.Errorf("read-only listing contains %q", unwanted)
		}
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, `/upload`, nil))
	if rec.Code != http.StatusOK || rec.Body.String() != `ordinary file` {
		t.Fatalf("read-only /upload file response = %d %q", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, `/upload`, nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("read-only upload POST status = %d, want 405", rec.Code)
	}
}

func TestHTTPSPageURLReplacesPortAndPreservesTarget(t *testing.T) {
	tests := []struct {
		target string
		host   string
		want   string
	}{
		{`http://serve.test:8000/dir/?sort=name`, `serve.test:8000`, `https://serve.test:8443/dir/?sort=name`},
		{`http://[2001:db8::1]:8000/a%20b`, `[2001:db8::1]:8000`, `https://[2001:db8::1]:8443/a%20b`},
		{`http://serve.test:8000/a%2Fb?`, `serve.test:8000`, `https://serve.test:8443/a%2Fb?`},
		{`http://localhost/plain`, `localhost`, `https://localhost:8443/plain`},
	}
	for _, tt := range tests {
		req := httptest.NewRequest(http.MethodGet, tt.target, nil)
		req.Host = tt.host
		if got := httpsPageURL(req); got != tt.want {
			t.Errorf("httpsPageURL(%q, host %q) = %q, want %q", tt.target, tt.host, got, tt.want)
		}
	}
}

func TestListingFileIconClassification(t *testing.T) {
	tests := []struct {
		name string
		want listingIcon
	}{
		{`README`, listingIconText},
		{`go.mod`, listingIconText},
		{`notes.TXT`, listingIconText},
		{`photo.jpeg`, listingIconImage},
		{`clip.mp4`, listingIconVideo},
		{`recording.flac`, listingIconAudio},
		{`manual.pdf`, listingIconDocument},
		{`drawing.svg`, listingIconImage},
		{`data.bin`, listingIconFile},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := listingFileIcon(tt.name); got != tt.want {
				t.Fatalf("listingFileIcon(%q) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

func TestDirectoryListingUsesFileTypeIconSprite(t *testing.T) {
	srv, dir := testServer(t)
	if err := os.Mkdir(filepath.Join(dir, `subdir`), 0o755); err != nil {
		t.Fatal(err)
	}
	files := []string{`notes.txt`, `photo.png`, `clip.mp4`, `song.mp3`, `manual.pdf`, `data.bin`}
	for _, name := range files {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	rec := httptest.NewRecorder()
	srv.page(rec, httptest.NewRequest(http.MethodGet, `/`, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("listing status = %d, body %q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, icon := range []listingIcon{
		listingIconFolder,
		listingIconText,
		listingIconImage,
		listingIconVideo,
		listingIconAudio,
		listingIconDocument,
		listingIconFile,
	} {
		if want := `href="#file-icon-` + string(icon) + `"`; !strings.Contains(body, want) {
			t.Errorf("listing does not use %q", want)
		}
	}
	if got, want := strings.Count(body, `class="file-type-icon"`), len(files)+1; got != want {
		t.Errorf("file icon count = %d, want %d", got, want)
	}
	if !strings.Contains(body, `class="icon-sprite" aria-hidden="true"`) {
		t.Error("icon sprite is not hidden from assistive technology")
	}
	if !strings.Contains(body, `class="file-type-icon" viewBox="0 0 16 16" width="16" height="16"`) {
		t.Error("file type icons do not have intrinsic dimensions")
	}
	if !strings.Contains(body, `class="barcode-transfer qr-transfer"`) ||
		!strings.Contains(body, `<svg viewBox="0 0 16 16" width="16" height="16" aria-hidden="true">`) {
		t.Error("QR transfer icons do not have intrinsic dimensions")
	}
}

func TestDirectoryListingSortsFoldersBeforeFiles(t *testing.T) {
	srv, dir := testServer(t)
	for _, name := range []string{`z-folder`, `m-folder`} {
		if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{`a-file.txt`, `b-file.txt`} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(`z-folder`, filepath.Join(dir, `n-linked-folder`)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	rec := httptest.NewRecorder()
	srv.page(rec, httptest.NewRequest(http.MethodGet, `/`, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("listing status = %d, body %q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	last := -1
	for _, name := range []string{
		`m-folder`,
		`n-linked-folder`,
		`z-folder`,
		`a-file.txt`,
		`b-file.txt`,
	} {
		index := strings.Index(body, name)
		if index < 0 {
			t.Fatalf("listing omits %q", name)
		}
		if index <= last {
			t.Fatalf("%q is out of order in listing", name)
		}
		last = index
	}
	if strings.Contains(body, `href="n-linked-folder`) {
		t.Error("directory symlink is navigable")
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

func TestCameraScannerProvidesRecoveryUI(t *testing.T) {
	srv, _ := testServer(t)
	handler := srv.mux()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, `http://serve.test:8000/?target=file`, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("page status = %d, body %q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`data-https-url="https://serve.test:8443/?target=file"`,
		`data-scanner-id="qr"`,
		`data-worker-url="/qrworker.js"`,
		`data-idle-prompt="point the camera at the QR codes"`,
		`data-transfer-label="QR"`,
		`aria-label="open QR scanner"`,
		`aria-label="received QR stream frames"`,
		`<strong id="camera-title">receive QR stream</strong>`,
		`class="camera-dialog"`,
		`class="scan-guide"`,
		`class="frame-progress"`,
		`class="camera-actions" hidden`,
		`class="save"`,
		`class="restart"`,
		`location.assign(cameraButton.dataset.httpsUrl)`,
		`let scannerBackend = Object.freeze({`,
		`worker = new Worker(scannerBackend.workerURL)`,
		`class="scanner-select"`,
		`data-worker-url="/jabworker.js"`,
		`data-dialog-title="receive JAB Code stream"`,
		`scannerSelect.addEventListener('change'`,
		`camProgress.textContent = scannerBackend.idlePrompt`,
		`if (worker !== scanWorker) return`,
		`worker.addEventListener('messageerror'`,
		`if (result.recoverable)`,
		`function restartDecoder()`,
		`const have = Math.min(total, Math.max(0, Number(result.have) || 0))`,
		`scanBusy = false`,
		`cameraGeneration++`,
		`clearInterval(scanTimer)`,
		`if (worker) worker.terminate()`,
		`if (camStream) for (const t of camStream.getTracks()) t.stop()`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scanner page does not contain %q", want)
		}
	}
	if strings.Contains(body, `location.hostname + ':8443'`) {
		t.Error("scanner page still constructs the HTTPS port in JavaScript")
	}

	for path, scanFunc := range map[string]string{
		`/qrworker.js`:  `qrstreamScanFrame`,
		`/jabworker.js`: `jabstreamScanFrame`,
	} {
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d", path, rec.Code)
		}
		worker := rec.Body.String()
		for _, want := range []string{
			scanFunc,
			`if (result.error)`,
			`recoverable: Boolean(result.recoverable)`,
			`recoverable: false`,
		} {
			if !strings.Contains(worker, want) {
				t.Errorf("%s does not contain %q", path, want)
			}
		}
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, `/jabstream.wasm`, nil))
	if rec.Code != http.StatusOK || rec.Header().Get(`Content-Type`) != `application/wasm` {
		t.Fatalf("jab wasm response = %d %q", rec.Code, rec.Header().Get(`Content-Type`))
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, `/style.css`, nil))
	styles := rec.Body.String()
	for _, want := range []string{`100dvh`, `object-fit: contain`, `.camera-actions[hidden]`} {
		if !strings.Contains(styles, want) {
			t.Errorf("scanner styles do not contain %q", want)
		}
	}
}

func TestQRScannerBackendDescriptor(t *testing.T) {
	want := scannerBackend{
		ID:            `qr`,
		WorkerURL:     `/qrworker.js`,
		DialogTitle:   `receive QR stream`,
		IdlePrompt:    `point the camera at the QR codes`,
		TransferLabel: `QR`,
	}
	if got := qrScannerBackend(false); got != want {
		t.Fatalf("QR scanner descriptor = %#v, want %#v", got, want)
	}
	want.WorkerURL += `?log=1`
	if got := qrScannerBackend(true); got != want {
		t.Fatalf("logged QR scanner descriptor = %#v, want %#v", got, want)
	}
}

func TestConfiguredTransferBackends(t *testing.T) {
	backends := configuredTransferBackends(false)
	if len(backends) != 2 {
		t.Fatalf("configured backends = %d, want 2", len(backends))
	}
	qr := backends[0]
	if !qr.SenderAvailable || !qr.ScannerAvailable || qr.ID != `qr` ||
		qr.SenderPathPrefix != `/qr/` || qr.WasmURL != `/qrstream.wasm` ||
		qr.Scanner != qrScannerBackend(false) {
		t.Fatalf("QR backend = %#v", qr)
	}
	jab := backends[1]
	if !jab.SenderAvailable || !jab.ScannerAvailable || jab.ID != `jab` ||
		jab.SenderPathPrefix != `/jab/` || jab.WasmURL != `/jabstream.wasm` ||
		jab.Scanner != jabScannerBackend(false) {
		t.Fatalf("JAB backend = %#v", jab)
	}
	if jab.Scanner.WorkerURL != `/jabworker.js` {
		t.Fatalf("JAB worker URL = %q", jab.Scanner.WorkerURL)
	}

	scanners := availableScanners(backends)
	if len(scanners) != 2 || scanners[0].ID != `qr` || scanners[1].ID != `jab` {
		t.Fatalf("available scanners = %#v", scanners)
	}
	for i := range backends {
		backends[i].ScannerAvailable = false
	}
	if scanners := availableScanners(backends); len(scanners) != 0 {
		t.Fatalf("scanners from unavailable backends: %#v", scanners)
	}
}

func TestTransferActionsRendered(t *testing.T) {
	srv, dir := testServer(t)
	name := `a & b.txt`
	if err := os.WriteFile(filepath.Join(dir, name), []byte(`content`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, `sub dir`), 0o755); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	srv.page(rec, httptest.NewRequest(http.MethodGet, `/`, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("page status = %d, body %q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	escaped := func(prefix, path string) string {
		href := (&url.URL{Path: prefix + path}).EscapedPath()
		return strings.ReplaceAll(href, `&`, `&amp;`)
	}
	for _, want := range []string{
		// file actions for both senders
		`class="barcode-transfer qr-transfer"`,
		`class="barcode-transfer jab-transfer"`,
		`data-transport-id="qr"`,
		`data-transport-id="jab"`,
		`href="` + escaped(`/qr/`, name) + `"`,
		`href="` + escaped(`/jab/`, name) + `"`,
		`aria-label="transfer a &amp; b.txt via QR codes"`,
		`aria-label="transfer a &amp; b.txt via JAB Code"`,
		// directory archive stream actions with combined icons
		`href="` + escaped(`/qr/`, `sub dir`) + `"`,
		`href="` + escaped(`/jab/`, `sub dir`) + `"`,
		`aria-label="transfer sub dir as a compressed archive via QR codes"`,
		`aria-label="transfer sub dir as a compressed archive via JAB Code"`,
		`href="#action-icon-qr-archive"`,
		`href="#action-icon-jab-archive"`,
		// current-directory stream actions beside the archive control
		`href="/qr/."`,
		`href="/jab/."`,
		`aria-label="transfer this directory as a compressed archive via QR codes"`,
		`aria-label="transfer this directory as a compressed archive via JAB Code"`,
		// shared dialog handling
		`document.querySelectorAll('.barcode-transfer')`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page does not contain %q", want)
		}
	}
	// QR stays the initial scanner; JAB is offered via the select
	if !strings.Contains(body, `data-scanner-id="qr"`) {
		t.Error("page does not select the QR scanner initially")
	}
	for _, want := range []string{
		`class="scanner-select"`,
		`<option value="qr"`,
		`<option value="jab"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page does not contain %q", want)
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
	if !strings.Contains(body, `fetch('/log'`) ||
		!strings.Contains(body, `data-worker-url="/qrworker.js?log=1"`) {
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
	if err := f.Truncate(maxBarcodeFileSize + 1); err != nil {
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

func TestTunnelInterface(t *testing.T) {
	for _, tt := range []struct {
		name   string
		flags  net.Flags
		tunnel bool
	}{
		{`eth0`, net.FlagUp | net.FlagBroadcast, false},
		{`wlan0`, net.FlagUp | net.FlagBroadcast, false},
		{`enp3s0`, net.FlagUp, false},
		{`tailscale0`, net.FlagUp | net.FlagPointToPoint, true},
		{`wg0`, net.FlagUp | net.FlagPointToPoint, true},
		{`tun0`, net.FlagUp, true},
		{`tap1`, net.FlagUp, true},
		{`utun3`, net.FlagUp, true},
		{`zt7nnig26`, net.FlagUp, true},
		{`ppp0`, net.FlagUp, true},
	} {
		if got := tunnelInterface(tt.name, tt.flags); got != tt.tunnel {
			t.Errorf("tunnelInterface(%q, %v) = %t, want %t", tt.name, tt.flags, got, tt.tunnel)
		}
	}
}

func TestCollectHostCandidates(t *testing.T) {
	ifaces := []ifaceAddrs{
		{name: `lo`, flags: net.FlagUp | net.FlagLoopback, ips: []net.IP{net.ParseIP(`127.0.0.1`), net.ParseIP(`::1`)}},
		{name: `eth0`, flags: net.FlagUp | net.FlagBroadcast, ips: []net.IP{
			net.ParseIP(`192.168.1.5`),
			net.ParseIP(`fe80::1`),
			net.ParseIP(`2001:db8::5`),
		}},
		{name: `tailscale0`, flags: net.FlagUp | net.FlagPointToPoint, ips: []net.IP{
			net.ParseIP(`100.64.0.3`),
			net.ParseIP(`fd7a:115c:a1e0::3`),
		}},
		{name: `wlan0`, flags: net.FlagUp | net.FlagBroadcast, ips: []net.IP{
			net.ParseIP(`10.0.0.7`),
			net.ParseIP(`10.0.0.7`), // duplicate must not repeat
		}},
	}
	// The default route leaves via wlan0, so its address must lead
	// even though eth0 is listed first.
	got := collectHostCandidates(net.ParseIP(`10.0.0.7`), ifaces)
	want := []hostCandidate{
		{host: `10.0.0.7`, iface: `wlan0`},
		{host: `192.168.1.5`, iface: `eth0`},
		{host: `100.64.0.3`, iface: `tailscale0`, tunnel: true},
		{host: `[2001:db8::5]`, iface: `eth0`},
		{host: `[fd7a:115c:a1e0::3]`, iface: `tailscale0`, tunnel: true},
		{host: `localhost`},
	}
	if len(got) != len(want) {
		t.Fatalf("candidates = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("candidate %d = %#v, want %#v", i, got[i], want[i])
		}
	}
}

func TestHostLabel(t *testing.T) {
	for _, tt := range []struct {
		candidate hostCandidate
		want      string
	}{
		{hostCandidate{host: `localhost`}, ``},
		{hostCandidate{host: `192.168.1.5`, iface: `eth0`}, ` (eth0)`},
		{hostCandidate{host: `100.64.0.3`, iface: `tailscale0`, tunnel: true}, ` (tailscale0, VPN/tunnel)`},
	} {
		if got := hostLabel(tt.candidate); got != tt.want {
			t.Errorf("hostLabel(%#v) = %q, want %q", tt.candidate, got, tt.want)
		}
	}
}

// flushCancelWriter cancels the request context on the first flush,
// so a streaming handler emits exactly one frame and then observes a
// client disconnect.
type flushCancelWriter struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
}

func (w *flushCancelWriter) Flush() {
	w.ResponseRecorder.Flush()
	w.cancel()
}

// barcodeStreamFirstFrame drives a barcode sender until its first
// flushed frame, cancels the request as a disconnecting client
// would, and decodes the emitted multipart PNG part.
func barcodeStreamFirstFrame(t *testing.T, srv *server, handler func(http.ResponseWriter, *http.Request), target string) image.Config {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, target, nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	handler(&flushCancelWriter{ResponseRecorder: rec, cancel: cancel}, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("stream status = %d, body %q", rec.Code, rec.Body.String())
	}
	mediaType, params, err := mime.ParseMediaType(rec.Header().Get(`Content-Type`))
	if err != nil || mediaType != `multipart/x-mixed-replace` {
		t.Fatalf("stream content type = %q (%v)", rec.Header().Get(`Content-Type`), err)
	}
	reader := multipart.NewReader(rec.Body, params[`boundary`])
	part, err := reader.NextPart()
	if err != nil {
		t.Fatalf("no multipart frame: %v", err)
	}
	if part.Header.Get(`Content-Type`) != `image/png` {
		t.Fatalf("frame content type = %q", part.Header.Get(`Content-Type`))
	}
	config, err := png.DecodeConfig(part)
	if err != nil {
		t.Fatalf("frame is not a PNG: %v", err)
	}
	return config
}

func TestJABSenderStreamsFileAndDirectory(t *testing.T) {
	srv, dir := testServer(t)
	if err := os.WriteFile(filepath.Join(dir, `payload.bin`), []byte(`jab payload`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, `tree`, `nested`), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, `tree`, `nested`, `f.txt`), []byte(`in archive`), 0o644); err != nil {
		t.Fatal(err)
	}

	// the default plan is fixed: version 20 is a 97-module square at
	// 8 px per module with a 2-module margin on each side
	const wantSize = (97 + 2*2) * 8
	for _, target := range []string{`/jab/payload.bin`, `/jab/tree`, `/jab/.`} {
		config := barcodeStreamFirstFrame(t, srv, srv.jab, target)
		if config.Width != wantSize || config.Height != wantSize {
			t.Fatalf("%s frame = %dx%d, want %dx%d", target, config.Width, config.Height, wantSize, wantSize)
		}
	}
}

func TestQRSenderStreamsDirectoryArchive(t *testing.T) {
	srv, dir := testServer(t)
	if err := os.Mkdir(filepath.Join(dir, `tree`), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, `tree`, `f.txt`), []byte(`in archive`), 0o644); err != nil {
		t.Fatal(err)
	}
	if config := barcodeStreamFirstFrame(t, srv, srv.qr, `/qr/tree`); config.Width != config.Height || config.Width == 0 {
		t.Fatalf("QR archive frame = %dx%d", config.Width, config.Height)
	}
}

func TestJABSenderMethodAndPathFailures(t *testing.T) {
	srv, dir := testServer(t)
	if err := os.Symlink(`.`, filepath.Join(dir, `linked-dir`)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	rec := httptest.NewRecorder()
	srv.jab(rec, httptest.NewRequest(http.MethodPost, `/jab/file`, nil))
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get(`Allow`) != http.MethodGet {
		t.Fatalf("wrong-method response = %d Allow=%q", rec.Code, rec.Header().Get(`Allow`))
	}

	for _, target := range []string{`/jab/`, `/jab/missing`, `/jab/../escape`, `/jab/linked-dir`} {
		rec := httptest.NewRecorder()
		srv.jab(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s status = %d, want 404", target, rec.Code)
		}
	}
}

func TestJABRejectsOversizedFile(t *testing.T) {
	srv, dir := testServer(t)
	f, err := os.Create(filepath.Join(dir, `large.bin`))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxBarcodeFileSize + 1); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	srv.jab(rec, httptest.NewRequest(http.MethodGet, `/jab/large.bin`, nil))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized JAB status = %d, want 413", rec.Code)
	}
}

func TestJABSenderColorSelection(t *testing.T) {
	srv, dir := testServer(t)
	if err := os.WriteFile(filepath.Join(dir, `f.bin`), []byte(`payload`), 0o644); err != nil {
		t.Fatal(err)
	}
	// values no build accepts; build-dependent modes are handled in
	// the loop below so this test cannot start an unbounded stream
	for _, target := range []string{
		`/jab/f.bin?colors=7`,
		`/jab/f.bin?colors=abc`,
		`/jab/f.bin?colors=512`,
		`/jab/f.bin?colors=-8`,
	} {
		rec := httptest.NewRecorder()
		srv.jab(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400", target, rec.Code)
		}
	}
	// one accepted value proves the parameter reaches the encoder;
	// per-mode encode and decode behavior belongs to the libraries.
	// 4 colors keeps the frame cheap and the geometry fixed.
	const wantSize = (97 + 2*2) * 8
	if config := barcodeStreamFirstFrame(t, srv, srv.jab, `/jab/f.bin?colors=4`); config.Width != wantSize || config.Height != wantSize {
		t.Errorf("4 colors frame = %dx%d, want %dx%d", config.Width, config.Height, wantSize, wantSize)
	}
	for _, colors := range []int{16, 32, 64, 128, 256} {
		if slices.Contains(jabstream.SupportedColors(), colors) {
			continue
		}
		rec := httptest.NewRecorder()
		srv.jab(rec, httptest.NewRequest(http.MethodGet, `/jab/f.bin?colors=`+strconv.Itoa(colors), nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("unsupported %d colors status = %d, want 400", colors, rec.Code)
		}
	}
}
