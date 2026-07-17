package main

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/klauspost/compress/zstd"
)

const testArchiveToken = `0123456789abcdef0123456789abcdef`

type archiveMember struct {
	header tar.Header
	data   []byte
}

func readArchive(t *testing.T, data []byte) map[string]archiveMember {
	t.Helper()
	decoder, err := zstd.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer decoder.Close()
	reader := tar.NewReader(decoder)
	members := make(map[string]archiveMember)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		members[header.Name] = archiveMember{*header, content}
	}
	return members
}

func readArchiveResult(t *testing.T, handler http.Handler, token string) archiveResult {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, `/.serve/archive-status?status=`+token, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("archive status = %d, body %q", recorder.Code, recorder.Body.String())
	}
	var result archiveResult
	if err := json.NewDecoder(recorder.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestArchiveDownloadStreamsDirectory(t *testing.T) {
	srv, directory := testServer(t)
	if err := os.MkdirAll(filepath.Join(directory, `tree`, `empty`), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, `tree`, `hello.txt`), []byte(`hello`), 0o640); err != nil {
		t.Fatal(err)
	}
	handler := srv.mux()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, `/.serve/archive?path=tree&status=`+testArchiveToken, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("archive status = %d, body %q", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get(`Content-Type`); got != `application/zstd` {
		t.Fatalf("archive content type = %q", got)
	}
	_, parameters, err := mime.ParseMediaType(recorder.Header().Get(`Content-Disposition`))
	if err != nil {
		t.Fatal(err)
	}
	if got := parameters[`filename`]; got != `tree.tar.zst` {
		t.Fatalf("archive filename = %q", got)
	}
	members := readArchive(t, recorder.Body.Bytes())
	if members[`tree/`].header.Typeflag != tar.TypeDir || members[`tree/empty/`].header.Typeflag != tar.TypeDir {
		t.Fatalf("archive directories missing: %#v", members)
	}
	if got := string(members[`tree/hello.txt`].data); got != `hello` {
		t.Fatalf("archived file = %q", got)
	}
	result := readArchiveResult(t, handler, testArchiveToken)
	if result.Running || result.Filename != `tree.tar.zst` || result.Warnings != 0 || result.Error != `` {
		t.Fatalf("archive result = %+v", result)
	}
}

func TestArchiveDownloadUsesProvidedFSWithoutWritableRoot(t *testing.T) {
	srv, err := newServer(
		fstest.MapFS{
			`tree`:          {Mode: fs.ModeDir | 0o755},
			`tree/file.txt`: {Data: []byte(`virtual`)},
		},
		nil,
		log.New(io.Discard, ``, 0),
		false,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	srv.mux().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, `/.serve/archive?path=tree`, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("archive status = %d, body %q", recorder.Code, recorder.Body.String())
	}
	if got := string(readArchive(t, recorder.Body.Bytes())[`tree/file.txt`].data); got != `virtual` {
		t.Fatalf("archived virtual file = %q", got)
	}
}

type failingOpenFS struct {
	fs.FS
	name string
}

func (f failingOpenFS) Open(name string) (fs.File, error) {
	if name == f.name {
		return nil, fs.ErrPermission
	}
	return f.FS.Open(name)
}

func TestArchiveWarningsUseIncompleteFilenameAndStatus(t *testing.T) {
	directory := t.TempDir()
	if err := os.Mkdir(filepath.Join(directory, `tree`), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, `tree`, `blocked.txt`), []byte(`blocked`), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	var logs bytes.Buffer
	srv, err := newServer(failingOpenFS{root.FS(), `tree/blocked.txt`}, root, log.New(&logs, ``, 0), false, 0)
	if err != nil {
		t.Fatal(err)
	}
	handler := srv.mux()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, `/.serve/archive?path=tree&status=`+testArchiveToken, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("archive status = %d, body %q", recorder.Code, recorder.Body.String())
	}
	_, parameters, err := mime.ParseMediaType(recorder.Header().Get(`Content-Disposition`))
	if err != nil {
		t.Fatal(err)
	}
	if got := parameters[`filename`]; got != `tree.incomplete.tar.zst` {
		t.Fatalf("archive filename = %q", got)
	}
	members := readArchive(t, recorder.Body.Bytes())
	if _, ok := members[`tree/blocked.txt.serve-error.txt`]; !ok {
		t.Fatalf("archive has no per-file error marker: %#v", members)
	}
	if _, ok := members[`tree/serve-archive-errors.txt`]; !ok {
		t.Fatalf("archive has no aggregate error report: %#v", members)
	}
	result := readArchiveResult(t, handler, testArchiveToken)
	if result.Warnings == 0 || result.Error != `` {
		t.Fatalf("archive result = %+v", result)
	}
	if got := logs.String(); !strings.Contains(got, `archive path="tree" member="tree/blocked.txt" warning="file open failed:`) {
		t.Fatalf("archive warning log = %q", got)
	}
}

func TestArchiveRoutesReserveInternalNamespace(t *testing.T) {
	srv, directory := testServer(t)
	if err := os.Mkdir(filepath.Join(directory, `.serve`), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, `.serve`, `archive`), []byte(`raw collision`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, `.serve`, `other`), []byte(`raw collision`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{`/.serve/archive`, `/.serve/other`} {
		recorder := httptest.NewRecorder()
		srv.mux().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
		if recorder.Code != http.StatusNotFound || strings.Contains(recorder.Body.String(), `raw collision`) {
			t.Fatalf("reserved route %q response = %d %q", target, recorder.Code, recorder.Body.String())
		}
	}
}

func TestArchiveDownloadHEADAndMethods(t *testing.T) {
	srv, directory := testServer(t)
	if err := os.Mkdir(filepath.Join(directory, `tree`), 0o755); err != nil {
		t.Fatal(err)
	}
	handler := srv.mux()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodHead, `/.serve/archive?path=tree`, nil))
	if recorder.Code != http.StatusOK || recorder.Body.Len() != 0 {
		t.Fatalf("archive HEAD = %d, %d bytes", recorder.Code, recorder.Body.Len())
	}
	if recorder.Header().Get(`Content-Disposition`) == `` {
		t.Fatal("archive HEAD has no filename")
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, `/.serve/archive?path=tree`, nil))
	if recorder.Code != http.StatusMethodNotAllowed || recorder.Header().Get(`Allow`) != `GET, HEAD` {
		t.Fatalf("archive POST = %d, Allow %q", recorder.Code, recorder.Header().Get(`Allow`))
	}
}

func TestArchiveDownloadDoesNotFollowDirectorySymlink(t *testing.T) {
	srv, directory := testServer(t)
	if err := os.Mkdir(filepath.Join(directory, `real`), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(`real`, filepath.Join(directory, `linked`)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	recorder := httptest.NewRecorder()
	srv.mux().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, `/.serve/archive?path=linked`, nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("symlink directory archive status = %d, want 404", recorder.Code)
	}
}

type disconnectedResponseWriter struct{ header http.Header }

func (w *disconnectedResponseWriter) Header() http.Header { return w.header }
func (*disconnectedResponseWriter) WriteHeader(int)       {}
func (*disconnectedResponseWriter) Write([]byte) (int, error) {
	return 0, errors.New(`client disconnected`)
}

func TestArchiveClientDisconnectIsReportedPerRequest(t *testing.T) {
	srv, directory := testServer(t)
	if err := os.Mkdir(filepath.Join(directory, `tree`), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, `tree`, `file`), bytes.Repeat([]byte(`x`), 1<<16), 0o644); err != nil {
		t.Fatal(err)
	}
	response := &disconnectedResponseWriter{header: make(http.Header)}
	srv.archive(response, httptest.NewRequest(http.MethodGet, `/.serve/archive?path=tree&status=`+testArchiveToken, nil))
	result := readArchiveResult(t, srv.mux(), testArchiveToken)
	if result.Error == `` || !strings.Contains(result.Error, `client disconnected`) {
		t.Fatalf("archive disconnect result = %+v", result)
	}
}

func TestDirectoryListingOffersArchiveDownloads(t *testing.T) {
	srv, directory := testServer(t)
	if err := os.Mkdir(filepath.Join(directory, `folder`), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, `file.txt`), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	srv.page(recorder, httptest.NewRequest(http.MethodGet, `/`, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("listing status = %d", recorder.Code)
	}
	body := recorder.Body.String()
	for _, want := range []string{
		`href="/.serve/archive?path=."`,
		`href="/.serve/archive?path=folder"`,
		`id="action-icon-archive"`,
		`class="file-action archive-download"`,
		`pollArchiveStatus(token, name)`,
		`alert(message + '; see the error report inside the archive')`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("listing does not contain %q", want)
		}
	}
	if got := strings.Count(body, `class="file-action archive-download"`); got != 2 {
		t.Fatalf("archive action count = %d, want 2", got)
	}
}
