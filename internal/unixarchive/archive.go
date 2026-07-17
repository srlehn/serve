// Package unixarchive streams a Unix directory as a PAX tar archive compressed
// with zstd.
package unixarchive

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/srlehn/serve/internal/payload"
)

const (
	maxWarnings   = 512
	maxReportSize = 1 << 20
	maxWarningLen = 4 << 10
)

// Options configures an archive source.
type Options struct {
	RootName string
	OSRoot   *os.Root
	Warning  func(payload.Warning)
}

// Source is a repeatable streaming directory archive.
type Source struct {
	files        fs.FS
	directory    string
	rootName     string
	metadata     metadataReader
	warning      func(payload.Warning)
	preflight    payload.Report
	preflightKey map[string]struct{}
}

// New validates directory and checks entries that can fail before streaming.
func New(files fs.FS, directory string, options Options) (*Source, error) {
	if files == nil || !fs.ValidPath(directory) {
		return nil, fs.ErrInvalid
	}
	info, err := lstat(files, directory)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("unixarchive: %s is not a directory", directory)
	}
	rootName := options.RootName
	if rootName == `` {
		rootName = path.Base(directory)
		if rootName == `.` {
			rootName = `archive`
		}
	}
	if !fs.ValidPath(rootName) || rootName == `.` || strings.Contains(rootName, `/`) {
		return nil, fmt.Errorf("unixarchive: invalid root name %q", rootName)
	}
	source := &Source{
		files:     files,
		directory: directory,
		rootName:  rootName,
		metadata:  metadataFor(options.OSRoot),
		warning:   options.Warning,
	}
	warnings := newWarnings(payload.Report{}, nil, source.warning)
	source.preflightEntries(warnings)
	source.preflight = warnings.report
	source.preflightKey = warnings.keys
	return source, nil
}

func (s *Source) Filename() string {
	if s.preflight.WarningCount() != 0 {
		return s.rootName + `.incomplete.tar.zst`
	}
	return s.rootName + `.tar.zst`
}

func (s *Source) Size() (int64, bool) { return 0, false }

// WriteTo streams the archive without buffering it or writing a temporary file.
func (s *Source) WriteTo(ctx context.Context, dst io.Writer) (payload.Report, error) {
	if dst == nil {
		return cloneReport(s.preflight), errors.New(`unixarchive: nil writer`)
	}
	warnings := newWarnings(cloneReport(s.preflight), s.preflightKey, s.warning)
	encoder, err := zstd.NewWriter(contextWriter{ctx, dst}, zstd.WithEncoderConcurrency(1))
	if err != nil {
		return warnings.report, err
	}
	tw := tar.NewWriter(encoder)
	writer := archiveWriter{
		source:    s,
		ctx:       ctx,
		tar:       tw,
		warnings:  warnings,
		hardlinks: make(map[fileKey]string),
		generated: make(map[string]struct{}),
	}
	walkErr := fs.WalkDir(s.files, s.directory, writer.writeEntry)
	if walkErr == nil && warnings.report.WarningCount() != 0 {
		walkErr = writer.writeReport()
	}
	return warnings.report, errors.Join(walkErr, tw.Close(), encoder.Close())
}

func (s *Source) preflightEntries(warnings *warningSet) {
	err := fs.WalkDir(s.files, s.directory, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			warnings.add(name, fmt.Sprintf("traversal failed: %v", walkErr))
			return nil
		}
		info, header, entryWarnings, err := s.header(name, entry)
		for _, warning := range entryWarnings {
			warnings.add(name, warning.Error())
		}
		if err != nil {
			warnings.add(name, err.Error())
			return nil
		}
		_ = header
		if info.Mode().IsRegular() {
			file, err := s.files.Open(name)
			if err != nil {
				warnings.add(name, fmt.Sprintf("file open failed: %v", err))
				return nil
			}
			if err := file.Close(); err != nil {
				warnings.add(name, fmt.Sprintf("file close failed: %v", err))
			}
		}
		return nil
	})
	if err != nil {
		warnings.add(s.directory, fmt.Sprintf("traversal stopped: %v", err))
	}
}

func (s *Source) header(name string, entry fs.DirEntry) (fs.FileInfo, *tar.Header, []error, error) {
	info, err := entry.Info()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("metadata read failed: %w", err)
	}
	link := ``
	if info.Mode()&fs.ModeSymlink != 0 {
		link, err = fs.ReadLink(s.files, name)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("symbolic link target read failed: %w", err)
		}
	}
	header, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("tar header creation failed: %w", err)
	}
	header.Name = s.archiveName(name)
	if info.IsDir() {
		header.Name += `/`
	}
	header.Uname = ``
	header.Gname = ``
	header.Format = tar.FormatPAX
	var warnings []error
	if s.metadata != nil {
		warnings = s.metadata.enrich(name, info, header)
	}
	return info, header, warnings, nil
}

func (s *Source) archiveName(name string) string {
	if name == s.directory {
		return s.rootName
	}
	return path.Join(s.rootName, strings.TrimPrefix(name, s.directory+`/`))
}

type archiveWriter struct {
	source    *Source
	ctx       context.Context
	tar       *tar.Writer
	warnings  *warningSet
	hardlinks map[fileKey]string
	generated map[string]struct{}
}

func (w *archiveWriter) writeEntry(name string, entry fs.DirEntry, walkErr error) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	if walkErr != nil {
		message := fmt.Sprintf("traversal failed: %v", walkErr)
		w.warnings.add(name, message)
		return w.writeMarker(name, w.source.archiveName(name), `entry or subtree omitted`, []string{message})
	}
	info, header, metadataWarnings, err := w.source.header(name, entry)
	entryWarnings := errorStrings(metadataWarnings)
	for _, warning := range entryWarnings {
		w.warnings.add(name, warning)
	}
	if err != nil {
		message := err.Error()
		w.warnings.add(name, message)
		return w.writeMarker(name, w.source.archiveName(name), `entry omitted`, append(entryWarnings, message))
	}

	key, links, identified := fileIdentity(info)
	if identified && links > 1 && !info.IsDir() {
		if target, ok := w.hardlinks[key]; ok {
			header.Typeflag = tar.TypeLink
			header.Linkname = target
			header.Size = 0
			if err := w.tar.WriteHeader(header); err != nil {
				return err
			}
			return w.writeMarker(name, header.Name, `metadata incomplete`, entryWarnings)
		}
	}

	if !info.Mode().IsRegular() {
		if err := w.tar.WriteHeader(header); err != nil {
			return err
		}
		if identified && links > 1 && !info.IsDir() {
			w.hardlinks[key] = strings.TrimSuffix(header.Name, `/`)
		}
		return w.writeMarker(name, header.Name, `metadata incomplete`, entryWarnings)
	}

	file, err := w.source.files.Open(name)
	if err != nil {
		message := fmt.Sprintf("file open failed: %v", err)
		w.warnings.add(name, message)
		return w.writeMarker(name, header.Name, `file omitted`, append(entryWarnings, message))
	}
	if err := w.tar.WriteHeader(header); err != nil {
		file.Close()
		return err
	}
	result, readWarnings, copyErr := w.copyFile(file, header.Size)
	if closeErr := file.Close(); closeErr != nil {
		readWarnings = append(readWarnings, fmt.Sprintf("file close failed: %v", closeErr))
	}
	if copyErr != nil {
		return copyErr
	}
	for _, warning := range readWarnings {
		w.warnings.add(name, warning)
	}
	entryWarnings = append(entryWarnings, readWarnings...)
	if identified && links > 1 {
		w.hardlinks[key] = header.Name
	}
	return w.writeMarker(name, header.Name, result, entryWarnings)
}

func (w *archiveWriter) copyFile(file fs.File, size int64) (string, []string, error) {
	n, err := io.CopyN(w.tar, contextReader{w.ctx, file}, size)
	if err != nil {
		if contextErr := w.ctx.Err(); contextErr != nil {
			return ``, nil, contextErr
		}
		missing := size - n
		if padErr := writeZeros(w.tar, missing); padErr != nil {
			return ``, nil, padErr
		}
		return `original member padded with zero bytes`, []string{fmt.Sprintf("file read ended with %d bytes missing: %v", missing, err)}, nil
	}
	var extra [1]byte
	extraN, err := file.Read(extra[:])
	if extraN != 0 {
		return `original member truncated to its initial size`, []string{`file grew while it was being archived`}, nil
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return `file content archived`, []string{fmt.Sprintf("file growth check failed: %v", err)}, nil
	}
	return `metadata incomplete`, nil, nil
}

func (w *archiveWriter) writeMarker(sourceName, archiveName, result string, messages []string) error {
	if len(messages) == 0 {
		return nil
	}
	sourceStem := sourceName + `.serve-error`
	archiveStem := strings.TrimSuffix(archiveName, `/`) + `.serve-error`
	if strings.TrimSuffix(archiveName, `/`) == w.source.rootName {
		sourceStem = path.Join(w.source.directory, `serve-error`)
		archiveStem = path.Join(w.source.rootName, `serve-error`)
	}
	name, ok := w.availableName(sourceStem, archiveStem)
	if !ok {
		w.warnings.add(sourceName, `could not choose a collision-safe error marker name`)
		return nil
	}
	var content strings.Builder
	fmt.Fprintf(&content, "source: %s\nresult: %s\n", sourceName, result)
	for _, message := range messages {
		fmt.Fprintf(&content, "- %s\n", message)
	}
	return w.writeText(name, content.String())
}

func (w *archiveWriter) writeReport() error {
	name, ok := w.availableName(path.Join(w.source.directory, `serve-archive-errors`), path.Join(w.source.rootName, `serve-archive-errors`))
	if !ok {
		w.warnings.add(w.source.directory, `could not choose a collision-safe archive report name`)
		return nil
	}
	var content strings.Builder
	fmt.Fprintf(&content, "serve archive completed with %d warning(s).\n\n", w.warnings.report.WarningCount())
	shown := 0
	for _, warning := range w.warnings.report.Warnings {
		line := fmt.Sprintf("%s: %s\n", warning.Path, warning.Message)
		if content.Len()+len(line)+128 > maxReportSize {
			break
		}
		content.WriteString(line)
		shown++
	}
	omitted := len(w.warnings.report.Warnings) - shown + w.warnings.report.OmittedWarnings
	if omitted != 0 {
		fmt.Fprintf(&content, "\n%d additional warning(s) omitted.\n", omitted)
	}
	return w.writeText(name, content.String())
}

func (w *archiveWriter) availableName(sourceStem, archiveStem string) (string, bool) {
	for number := range 1000 {
		suffix := `.txt`
		if number != 0 {
			suffix = fmt.Sprintf(".%d.txt", number)
		}
		candidateArchive := archiveStem + suffix
		if _, exists := w.generated[candidateArchive]; exists {
			continue
		}
		if _, err := lstat(w.source.files, sourceStem+suffix); !errors.Is(err, fs.ErrNotExist) {
			continue
		}
		w.generated[candidateArchive] = struct{}{}
		return candidateArchive, true
	}
	return ``, false
}

func (w *archiveWriter) writeText(name, content string) error {
	header := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), ModTime: time.Now().UTC(), Typeflag: tar.TypeReg, Format: tar.FormatPAX}
	if err := w.tar.WriteHeader(header); err != nil {
		return err
	}
	_, err := io.WriteString(w.tar, content)
	return err
}

type warningSet struct {
	report  payload.Report
	keys    map[string]struct{}
	handler func(payload.Warning)
}

func newWarnings(report payload.Report, keys map[string]struct{}, handler func(payload.Warning)) *warningSet {
	set := &warningSet{report: report, keys: make(map[string]struct{}, len(keys)), handler: handler}
	for key := range keys {
		set.keys[key] = struct{}{}
	}
	return set
}

func (w *warningSet) add(name, message string) {
	if len(message) > maxWarningLen {
		message = message[:maxWarningLen] + `...`
	}
	warning := payload.Warning{Path: name, Message: message}
	key := name + "\x00" + message
	if _, exists := w.keys[key]; exists {
		return
	}
	if len(w.keys) < maxWarnings*2 {
		w.keys[key] = struct{}{}
	}
	if w.handler != nil {
		w.handler(warning)
	}
	if len(w.report.Warnings) == maxWarnings {
		w.report.OmittedWarnings++
		return
	}
	w.report.Warnings = append(w.report.Warnings, warning)
}

func errorStrings(errs []error) []string {
	strings := make([]string, len(errs))
	for index, err := range errs {
		strings[index] = err.Error()
	}
	return strings
}

func writeZeros(dst io.Writer, count int64) error {
	var zeros [32 << 10]byte
	for count > 0 {
		chunk := zeros[:]
		if int64(len(chunk)) > count {
			chunk = chunk[:count]
		}
		if _, err := dst.Write(chunk); err != nil {
			return err
		}
		count -= int64(len(chunk))
	}
	return nil
}

func cloneReport(report payload.Report) payload.Report {
	report.Warnings = append([]payload.Warning(nil), report.Warnings...)
	return report
}

func lstat(files fs.FS, name string) (fs.FileInfo, error) {
	if links, ok := files.(fs.ReadLinkFS); ok {
		return links.Lstat(name)
	}
	return fs.Stat(files, name)
}

type contextWriter struct {
	ctx context.Context
	io.Writer
}

func (w contextWriter) Write(data []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.Writer.Write(data)
}

type contextReader struct {
	ctx context.Context
	io.Reader
}

func (r contextReader) Read(data []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(data)
}

type metadataReader interface {
	enrich(string, fs.FileInfo, *tar.Header) []error
}
