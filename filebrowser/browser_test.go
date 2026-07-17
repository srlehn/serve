package filebrowser

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

func testHandler(t *testing.T, files fs.FS) *Handler {
	t.Helper()
	handler, err := NewHandler(
		files,
		WithRenderer(func(w io.Writer, _ *http.Request, directory Directory) error {
			for _, entry := range directory.Entries {
				if _, err := io.WriteString(w, entry.Name+"\n"); err != nil {
					return err
				}
			}
			return nil
		}),
		WithContentType(`text/plain; charset=utf-8`),
	)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func TestNewHandlerNeedsOnlyFS(t *testing.T) {
	handler, err := NewHandler(fstest.MapFS{
		`file.txt`:          {Data: []byte(`data`)},
		`folder/inside.txt`: {Data: []byte(`inside`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, `/`, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("default listing status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`href="folder/"`, `>folder/</a>`, `href="file.txt"`, `>file.txt</a>`} {
		if !strings.Contains(body, want) {
			t.Errorf("default listing does not contain %q", want)
		}
	}
}

func TestHandlerServesFSDirectoriesAndRawFiles(t *testing.T) {
	files := fstest.MapFS{
		`a-file.txt`:          {Data: []byte(`plain`)},
		`page.html`:           {Data: []byte(`<h1>inert</h1>`)},
		`z-folder/inside.txt`: {Data: []byte(`inside`)},
	}
	handler := testHandler(t, files)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, `/`, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("listing status = %d, body %q", rec.Code, rec.Body.String())
	}
	if got, want := rec.Body.String(), "z-folder\na-file.txt\npage.html\n"; got != want {
		t.Fatalf("listing = %q, want %q", got, want)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, `/page.html`, nil))
	if rec.Code != http.StatusOK || rec.Body.String() != `<h1>inert</h1>` {
		t.Fatalf("raw file response = %d %q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(`Content-Type`); got != `text/plain; charset=utf-8` {
		t.Fatalf("raw file content type = %q", got)
	}
	if got := rec.Header().Get(`X-Content-Type-Options`); got != `nosniff` {
		t.Fatalf("X-Content-Type-Options = %q", got)
	}

	req := httptest.NewRequest(http.MethodGet, `/a-file.txt`, nil)
	req.Header.Set(`Range`, `bytes=1-3`)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusPartialContent || rec.Body.String() != `lai` {
		t.Fatalf("range response = %d %q", rec.Code, rec.Body.String())
	}
}

func TestHandlerRedirectsDirectoriesAndValidatesMethods(t *testing.T) {
	handler := testHandler(t, fstest.MapFS{`folder/file`: {Data: []byte(`x`)}})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, `/folder?value=1`, nil))
	if rec.Code != http.StatusMovedPermanently || rec.Header().Get(`Location`) != `/folder/?value=1` {
		t.Fatalf("directory redirect = %d %q", rec.Code, rec.Header().Get(`Location`))
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, `/`, nil))
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get(`Allow`) != `GET, HEAD` {
		t.Fatalf("wrong-method response = %d Allow=%q", rec.Code, rec.Header().Get(`Allow`))
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, `/missing`, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing response = %d", rec.Code)
	}
}

func TestHandlerReportsRenderErrorsWithoutPartialOutput(t *testing.T) {
	wantErr := errors.New(`render failed`)
	var handled error
	handler, err := NewHandler(
		fstest.MapFS{`file`: {Data: nil}},
		WithRenderer(func(w io.Writer, _ *http.Request, _ Directory) error {
			_, _ = io.WriteString(w, `partial output`)
			return wantErr
		}),
		WithErrorHandler(func(w http.ResponseWriter, _ *http.Request, status int, message string, err error) {
			handled = err
			http.Error(w, message, status)
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, `/`, nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("render failure status = %d", rec.Code)
	}
	if !errors.Is(handled, wantErr) {
		t.Fatalf("handled error = %v, want %v", handled, wantErr)
	}
	if strings.Contains(rec.Body.String(), `partial output`) {
		t.Fatalf("render failure leaked partial output: %q", rec.Body.String())
	}
}

func TestReadDirSortsFoldersBeforeFiles(t *testing.T) {
	entries, err := ReadDir(fstest.MapFS{
		`z-file`:       {Data: nil},
		`a-file`:       {Data: nil},
		`z-dir/inside`: {Data: nil},
		`a-dir/inside`: {Data: nil},
		`m-dir/inside`: {Data: nil},
	}, `.`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name)
	}
	if want := []string{`a-dir`, `m-dir`, `z-dir`, `a-file`, `z-file`}; !reflect.DeepEqual(names, want) {
		t.Fatalf("entry order = %q, want %q", names, want)
	}
}

type streamFS struct{ fs.FS }

func (s streamFS) Open(name string) (fs.File, error) {
	f, err := s.FS.Open(name)
	if err != nil {
		return nil, err
	}
	if dir, ok := f.(fs.ReadDirFile); ok {
		return streamDir{File: f, dir: dir}, nil
	}
	return streamFile{File: f}, nil
}

type streamFile struct{ fs.File }

type streamDir struct {
	fs.File
	dir fs.ReadDirFile
}

func (d streamDir) ReadDir(n int) ([]fs.DirEntry, error) { return d.dir.ReadDir(n) }

func TestHandlerStreamsNonSeekableFilesWithoutDroppingSniffedBytes(t *testing.T) {
	data := bytes.Repeat([]byte(`unknown text `), 100)
	handler := testHandler(t, streamFS{fstest.MapFS{`unknown`: {Data: data}}})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, `/unknown`, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("stream response = %d", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), data) {
		t.Fatalf("streamed body length = %d, want %d", rec.Body.Len(), len(data))
	}
	if got := rec.Header().Get(`Content-Type`); !strings.HasPrefix(got, `text/plain`) {
		t.Fatalf("stream content type = %q", got)
	}
}
