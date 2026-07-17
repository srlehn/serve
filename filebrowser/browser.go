// Package filebrowser serves an fs.FS as raw files and rendered directory
// listings.
//
// The supplied filesystem defines its own security boundary. For local
// directories where symlinks must not escape the tree, prefer os.Root.FS over
// os.DirFS. Directory symlinks are listed but are never traversed by Handler.
package filebrowser

import (
	"bytes"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
)

// ErrSymlinkDirectory reports an attempted traversal through a symlink whose
// target is a directory.
var ErrSymlinkDirectory = errors.New(`symlink directories are not served`)

// Entry describes one child in a directory listing. Mode describes the target
// for a usable symlink and the entry itself otherwise.
type Entry struct {
	Name    string
	Path    string
	Href    string
	Mode    fs.FileMode
	Symlink bool
	Err     error
}

// IsDir reports whether the entry is a real directory or a symlink to one.
func (e Entry) IsDir() bool { return e.Err == nil && e.Mode.IsDir() }

// IsRegular reports whether the entry is a real file or a symlink to one.
func (e Entry) IsRegular() bool { return e.Err == nil && e.Mode.IsRegular() }

// Note describes why an entry cannot be opened from a listing.
func (e Entry) Note() string {
	switch {
	case e.Err != nil && e.Symlink:
		return `unavailable symlink`
	case e.Err != nil:
		return `unavailable file`
	case e.IsDir() && e.Symlink:
		return `symlink directory not served`
	case !e.IsDir() && !e.IsRegular():
		return `special file not served`
	default:
		return ``
	}
}

// Directory is the filesystem name and sorted contents of one directory.
type Directory struct {
	Name    string
	Entries []Entry
}

// Renderer writes a directory representation. Handler buffers the result so a
// rendering failure can still produce a clean per-request error response.
type Renderer func(io.Writer, *http.Request, Directory) error

// ErrorHandler reports an internal request failure. It should log err as
// appropriate and write an HTTP response with status and message.
type ErrorHandler func(http.ResponseWriter, *http.Request, int, string, error)

// Options configures a Handler during construction.
type Options func(*Handler) error

// WithRenderer replaces the default directory renderer.
func WithRenderer(render Renderer) Options {
	return func(handler *Handler) error {
		if render == nil {
			return errors.New(`filebrowser: nil directory renderer`)
		}
		handler.render = render
		return nil
	}
}

// WithContentType sets the media type of rendered directory listings.
func WithContentType(contentType string) Options {
	return func(handler *Handler) error {
		if contentType == `` {
			return errors.New(`filebrowser: empty directory content type`)
		}
		handler.contentType = contentType
		return nil
	}
}

// WithErrorHandler sets the callback for internal request failures.
func WithErrorHandler(errorHandler ErrorHandler) Options {
	return func(handler *Handler) error {
		if errorHandler == nil {
			return errors.New(`filebrowser: nil error handler`)
		}
		handler.errorHandler = errorHandler
		return nil
	}
}

// Handler serves files and directory listings from an fs.FS.
type Handler struct {
	files        fs.FS
	render       Renderer
	contentType  string
	errorHandler ErrorHandler
}

// NewHandler returns a file browser backed by files. With no options it uses a
// self-contained default directory listing.
func NewHandler(files fs.FS, opts ...Options) (*Handler, error) {
	if files == nil {
		return nil, errors.New(`filebrowser: nil filesystem`)
	}
	handler := &Handler{
		files:       files,
		render:      renderDefault,
		contentType: `text/html; charset=utf-8`,
	}
	for _, option := range opts {
		if option == nil {
			return nil, errors.New(`filebrowser: nil option`)
		}
		if err := option(handler); err != nil {
			return nil, err
		}
	}
	return handler, nil
}

var defaultListing = template.Must(template.New(`directory`).Parse(`<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>files</title></head>
<body>
{{if ne .Name "."}}<p><a href="../">../</a></p>{{end}}
<ul>
{{range .Entries}}<li>{{if .Href}}<a href="{{.Href}}">{{.Name}}{{if .IsDir}}/{{end}}</a>{{else}}{{.Name}}{{if .IsDir}}/{{end}}{{end}}{{if .Note}} <small>({{.Note}})</small>{{end}}</li>
{{end}}</ul>
</body>
</html>
`))

func renderDefault(w io.Writer, _ *http.Request, directory Directory) error {
	return defaultListing.Execute(w, directory)
}

// ServeHTTP serves a raw regular file or a rendered directory listing.
func (h *Handler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}
	name, err := requestName(req.URL.Path)
	if err != nil {
		http.NotFound(w, req)
		return
	}
	f, info, err := Open(h.files, name)
	if err != nil {
		http.NotFound(w, req)
		return
	}
	defer f.Close()
	if !info.IsDir() {
		h.serveFile(w, req, f, info)
		return
	}
	if name != `.` && !strings.HasSuffix(req.URL.Path, `/`) {
		target := req.URL.Path + `/`
		if req.URL.RawQuery != `` {
			target += `?` + req.URL.RawQuery
		}
		http.Redirect(w, req, target, http.StatusMovedPermanently)
		return
	}
	entries, err := ReadDir(h.files, name)
	if err != nil {
		h.fail(w, req, http.StatusInternalServerError, `could not read directory`, err)
		return
	}
	var body bytes.Buffer
	if err := h.render(&body, req, Directory{Name: name, Entries: entries}); err != nil {
		h.fail(w, req, http.StatusInternalServerError, `could not render directory`, err)
		return
	}
	w.Header().Set(`Content-Type`, h.contentType)
	if req.Method == http.MethodGet {
		_, _ = body.WriteTo(w)
	}
}

func (h *Handler) fail(w http.ResponseWriter, req *http.Request, status int, message string, err error) {
	if h.errorHandler != nil {
		h.errorHandler(w, req, status, message, err)
		return
	}
	http.Error(w, message, status)
}

// Open opens name after rejecting directory symlinks in every path component.
// The root directory is named "." as required by fs.FS.
func Open(files fs.FS, name string) (fs.File, fs.FileInfo, error) {
	if files == nil || !fs.ValidPath(name) {
		return nil, nil, fs.ErrInvalid
	}
	if err := rejectSymlinkDirectories(files, name); err != nil {
		return nil, nil, err
	}
	f, err := files.Open(name)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, info, nil
}

func rejectSymlinkDirectories(files fs.FS, name string) error {
	if name == `.` {
		return nil
	}
	components := strings.Split(name, `/`)
	parent := `.`
	for i, component := range components {
		entries, err := fs.ReadDir(files, parent)
		if err != nil {
			return err
		}
		index := sort.Search(len(entries), func(i int) bool {
			return entries[i].Name() >= component
		})
		if index == len(entries) || entries[index].Name() != component {
			return fs.ErrNotExist
		}
		entry := entries[index]
		current := path.Join(parent, component)
		if entry.Type()&fs.ModeSymlink != 0 {
			if i != len(components)-1 {
				return fmt.Errorf("%w: %s", ErrSymlinkDirectory, current)
			}
			target, err := fs.Stat(files, current)
			if err != nil {
				return err
			}
			if target.IsDir() {
				return fmt.Errorf("%w: %s", ErrSymlinkDirectory, current)
			}
		}
		parent = current
	}
	return nil
}

// ReadDir returns directory entries with folders first and names sorted within
// the folder and non-folder groups. Directory symlinks have no Href and are not
// traversable.
func ReadDir(files fs.FS, name string) ([]Entry, error) {
	if files == nil || !fs.ValidPath(name) {
		return nil, fs.ErrInvalid
	}
	if err := rejectSymlinkDirectories(files, name); err != nil {
		return nil, err
	}
	children, err := fs.ReadDir(files, name)
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(children))
	for _, child := range children {
		entry := Entry{
			Name: child.Name(),
			Path: path.Join(name, child.Name()),
		}
		if child.Type()&fs.ModeSymlink != 0 {
			entry.Symlink = true
			info, err := fs.Stat(files, entry.Path)
			if err != nil {
				entry.Mode = child.Type()
				entry.Err = err
			} else {
				entry.Mode = info.Mode()
			}
		} else {
			info, err := child.Info()
			if err != nil {
				entry.Mode = child.Type()
				entry.Err = err
			} else {
				entry.Mode = info.Mode()
			}
		}
		switch {
		case entry.Err != nil:
		case entry.IsDir() && !entry.Symlink:
			entry.Href = url.PathEscape(entry.Name) + `/`
		case entry.IsRegular():
			entry.Href = url.PathEscape(entry.Name)
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir() != entries[j].IsDir() {
			return entries[i].IsDir()
		}
		return entries[i].Name < entries[j].Name
	})
	return entries, nil
}

func requestName(urlPath string) (string, error) {
	name := strings.TrimPrefix(urlPath, `/`)
	if name == urlPath {
		return ``, fs.ErrInvalid
	}
	name = strings.TrimSuffix(name, `/`)
	if name == `` {
		name = `.`
	}
	if !fs.ValidPath(name) {
		return ``, fs.ErrInvalid
	}
	return name, nil
}

func (h *Handler) serveFile(w http.ResponseWriter, req *http.Request, f fs.File, info fs.FileInfo) {
	if !info.Mode().IsRegular() {
		http.NotFound(w, req)
		return
	}
	contentType, reader := rawFileContent(f, info.Name())
	w.Header().Set(`Content-Type`, contentType)
	w.Header().Set(`X-Content-Type-Options`, `nosniff`)
	if seeker, ok := reader.(io.ReadSeeker); ok {
		http.ServeContent(w, req, info.Name(), info.ModTime(), seeker)
		return
	}
	if !info.ModTime().IsZero() {
		w.Header().Set(`Last-Modified`, info.ModTime().UTC().Format(http.TimeFormat))
	}
	if info.Size() >= 0 {
		w.Header().Set(`Content-Length`, strconv.FormatInt(info.Size(), 10))
	}
	if req.Method == http.MethodGet {
		_, _ = io.Copy(w, reader)
	}
}

func rawFileContent(f fs.File, name string) (string, io.Reader) {
	byExtension := mime.TypeByExtension(strings.ToLower(path.Ext(name)))
	if mediaType, _, err := mime.ParseMediaType(byExtension); err == nil {
		if IsTextMediaType(mediaType) {
			return `text/plain; charset=utf-8`, f
		}
		return byExtension, f
	}
	var head [512]byte
	n, err := f.Read(head[:])
	if err != nil && err != io.EOF {
		return `application/octet-stream`, io.MultiReader(bytes.NewReader(head[:n]), f)
	}
	contentType := http.DetectContentType(head[:n])
	if mediaType, _, err := mime.ParseMediaType(contentType); err == nil && IsTextMediaType(mediaType) {
		contentType = `text/plain; charset=utf-8`
	}
	if seeker, ok := f.(io.Seeker); ok {
		if _, err := seeker.Seek(0, io.SeekStart); err == nil {
			return contentType, f
		}
	}
	return contentType, io.MultiReader(bytes.NewReader(head[:n]), f)
}

// IsTextMediaType reports content types that raw file downloads should serve
// as inert text rather than active browser content.
func IsTextMediaType(mediaType string) bool {
	if strings.HasPrefix(mediaType, `text/`) {
		return true
	}
	switch mediaType {
	case `application/ecmascript`, `application/javascript`, `application/json`, `application/xml`:
		return true
	}
	return strings.HasSuffix(mediaType, `+json`) || strings.HasSuffix(mediaType, `+xml`)
}

func methodNotAllowed(w http.ResponseWriter, methods ...string) {
	w.Header().Set(`Allow`, strings.Join(methods, `, `))
	http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
}
