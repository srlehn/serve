package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"sync"
	"time"

	"github.com/srlehn/serve/internal/payload"
	"github.com/srlehn/serve/internal/unixarchive"
)

const (
	maxArchiveStatuses = 256
	archiveStatusTTL   = 10 * time.Minute
)

var archiveStatusToken = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)

type archiveResult struct {
	Running  bool   `json:"running"`
	Filename string `json:"filename,omitempty"`
	Warnings int    `json:"warnings,omitempty"`
	Error    string `json:"error,omitempty"`
	updated  time.Time
}

type archiveStatusStore struct {
	mu      sync.Mutex
	results map[string]archiveResult
}

func (s *archiveStatusStore) begin(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanup(time.Now())
	if s.results == nil {
		s.results = make(map[string]archiveResult)
	}
	if _, exists := s.results[token]; exists {
		return false
	}
	if len(s.results) >= maxArchiveStatuses {
		var oldestToken string
		var oldest time.Time
		for candidate, result := range s.results {
			if result.Running || !oldest.IsZero() && !result.updated.Before(oldest) {
				continue
			}
			oldestToken, oldest = candidate, result.updated
		}
		if oldestToken == `` {
			return false
		}
		delete(s.results, oldestToken)
	}
	s.results[token] = archiveResult{Running: true, updated: time.Now()}
	return true
}

func (s *archiveStatusStore) finish(token, filename string, report payload.Report, err error) {
	if token == `` {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result, exists := s.results[token]
	if !exists {
		return
	}
	result.Running = false
	result.Filename = filename
	result.Warnings = report.WarningCount()
	if err != nil {
		result.Error = err.Error()
	}
	result.updated = time.Now()
	s.results[token] = result
}

func (s *archiveStatusStore) get(token string) (archiveResult, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanup(time.Now())
	result, ok := s.results[token]
	return result, ok
}

func (s *archiveStatusStore) cleanup(now time.Time) {
	for token, result := range s.results {
		if !result.Running && now.Sub(result.updated) > archiveStatusTTL {
			delete(s.results, token)
		}
	}
}

func (s *server) archive(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}
	directory := req.URL.Query().Get(`path`)
	if !fs.ValidPath(directory) {
		http.NotFound(w, req)
		return
	}
	token := req.URL.Query().Get(`status`)
	if token != `` && !archiveStatusToken.MatchString(token) {
		http.Error(w, `invalid archive status token`, http.StatusBadRequest)
		return
	}
	if req.Method == http.MethodHead {
		token = ``
	}
	if token != `` && !s.archiveStatuses.begin(token) {
		http.Error(w, `archive status tracking is unavailable`, http.StatusServiceUnavailable)
		return
	}

	var source *unixarchive.Source
	var report payload.Report
	var archiveErr error
	defer func() {
		filename := ``
		if source != nil {
			filename = source.Filename()
		}
		s.archiveStatuses.finish(token, filename, report, archiveErr)
	}()

	source, archiveErr = unixarchive.New(s.files, directory, unixarchive.Options{
		RootName: s.archiveRootName(directory),
		OSRoot:   s.archiveRoot,
		Warning: func(warning payload.Warning) {
			s.logger.Printf("archive path=%q member=%q warning=%q", directory, warning.Path, warning.Message)
		},
	})
	if archiveErr != nil {
		if errors.Is(archiveErr, fs.ErrNotExist) || errors.Is(archiveErr, fs.ErrInvalid) {
			http.NotFound(w, req)
			return
		}
		s.requestError(w, req, http.StatusNotFound, `could not archive directory`, archiveErr)
		return
	}

	disposition := mime.FormatMediaType(`attachment`, map[string]string{`filename`: source.Filename()})
	if disposition == `` {
		disposition = `attachment`
	}
	w.Header().Set(`Content-Disposition`, disposition)
	w.Header().Set(`Content-Type`, `application/zstd`)
	w.Header().Set(`Cache-Control`, `no-store`)
	w.Header().Set(`X-Content-Type-Options`, `nosniff`)
	if req.Method == http.MethodHead {
		return
	}
	w.WriteHeader(http.StatusOK)
	report, archiveErr = source.WriteTo(req.Context(), w)
	if archiveErr != nil && !errors.Is(archiveErr, req.Context().Err()) {
		s.logger.Printf("archive path=%q error=%q", directory, archiveErr.Error())
	}
}

func (s *server) archiveStatus(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}
	token := req.URL.Query().Get(`status`)
	if !archiveStatusToken.MatchString(token) {
		http.NotFound(w, req)
		return
	}
	result, ok := s.archiveStatuses.get(token)
	if !ok {
		http.NotFound(w, req)
		return
	}
	w.Header().Set(`Cache-Control`, `no-store`)
	w.Header().Set(`Content-Type`, `application/json`)
	if result.Running {
		w.WriteHeader(http.StatusAccepted)
	}
	if req.Method == http.MethodGet {
		_ = json.NewEncoder(w).Encode(result)
	}
}

func (s *server) archiveRootName(directory string) string {
	if directory != `.` {
		return path.Base(directory)
	}
	if s.archiveRoot != nil {
		rootName := s.archiveRoot.Name()
		if absolute, err := filepath.Abs(rootName); err == nil {
			rootName = absolute
		}
		if name := filepath.Base(rootName); fs.ValidPath(name) && name != `.` {
			return name
		}
	}
	return `archive`
}

func sameFileSystem(left, right fs.FS) bool {
	leftValue, rightValue := reflect.ValueOf(left), reflect.ValueOf(right)
	return leftValue.IsValid() && rightValue.IsValid() &&
		leftValue.Type() == rightValue.Type() && leftValue.Comparable() &&
		leftValue.Interface() == rightValue.Interface()
}
