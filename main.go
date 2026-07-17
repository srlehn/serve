package main

import (
	"bytes"
	"compress/gzip"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	_ "embed"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"math/big"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"
	"github.com/srlehn/serve/filebrowser"
	"github.com/srlehn/serve/internal/payload"
	"github.com/srlehn/serve/qrstream"
)

const (
	httpPort      = `:8000`
	httpsPort     = `:8443`
	maxQRFileSize = 64 << 20
	maxClientLog  = 16 << 10
)

type byteSizeValue int64

func (v *byteSizeValue) String() string {
	if v == nil || *v == 0 {
		return `0`
	}
	return fmt.Sprintf(`%dB`, *v)
}

func (v *byteSizeValue) Set(value string) error {
	size, err := parseByteSize(value)
	if err != nil {
		return err
	}
	*v = byteSizeValue(size)
	return nil
}

func parseByteSize(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == `` {
		return 0, errors.New(`size is empty`)
	}

	i := 0
	for i < len(value) && (value[i] >= '0' && value[i] <= '9' || value[i] == '.') {
		i++
	}
	numberText := value[:i]
	unit := strings.ToLower(strings.TrimSpace(value[i:]))
	number, ok := new(big.Rat).SetString(numberText)
	if !ok || number.Sign() < 0 {
		return 0, fmt.Errorf("invalid size %q", value)
	}
	if unit == `` {
		if number.Sign() == 0 {
			return 0, nil
		}
		return 0, fmt.Errorf("size %q needs a unit such as MB, GB, MiB, or GiB", value)
	}
	factors := map[string]int64{
		`b`:   1,
		`kb`:  1000,
		`mb`:  1000 * 1000,
		`gb`:  1000 * 1000 * 1000,
		`tb`:  1000 * 1000 * 1000 * 1000,
		`kib`: 1 << 10,
		`mib`: 1 << 20,
		`gib`: 1 << 30,
		`tib`: 1 << 40,
	}
	factor, ok := factors[unit]
	if !ok {
		return 0, fmt.Errorf("unknown size unit %q", value[i:])
	}
	number.Mul(number, new(big.Rat).SetInt64(factor))
	if !number.IsInt() {
		return 0, fmt.Errorf("size %q is not a whole number of bytes", value)
	}
	bytes := number.Num()
	if !bytes.IsInt64() {
		return 0, fmt.Errorf("size %q is too large", value)
	}
	return bytes.Int64(), nil
}

//go:embed index.html.template
var pageTemplate string

//go:embed style.css
var styleCSS []byte

// the camera scanner: the qrstream decoder compiled to wasm plus
// the matching JS loader. Rebuild with go generate.
//
//go:generate go run wasm/generate.go
var (
	//go:embed wasm/qrstream.wasm
	qrWASM []byte
	//go:embed wasm/wasm_exec.js
	wasmExec []byte
	//go:embed wasm/worker.js
	qrWorker []byte
)

type server struct {
	files           fs.FS
	uploadRoot      *os.Root
	archiveRoot     *os.Root
	browser         *filebrowser.Handler
	template        *template.Template
	logger          *log.Logger
	browserLogging  bool
	uploadLimit     int64
	archiveStatuses archiveStatusStore
}

func newServer(files fs.FS, uploadRoot *os.Root, logger *log.Logger, browserLogging bool, uploadLimit int64) (*server, error) {
	if uploadLimit < 0 {
		return nil, errors.New(`upload limit must not be negative`)
	}
	tp, err := template.New(`index`).Parse(pageTemplate)
	if err != nil {
		return nil, err
	}
	s := &server{
		files:          files,
		uploadRoot:     uploadRoot,
		template:       tp,
		logger:         logger,
		browserLogging: browserLogging,
		uploadLimit:    uploadLimit,
	}
	if uploadRoot != nil && sameFileSystem(files, uploadRoot.FS()) {
		s.archiveRoot = uploadRoot
	}
	s.browser, err = filebrowser.NewHandler(
		files,
		filebrowser.WithRenderer(s.renderDirectory),
		filebrowser.WithErrorHandler(s.requestError),
	)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (s *server) mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.page)
	mux.HandleFunc("/.serve/archive", s.archive)
	mux.HandleFunc("/.serve/archive-status", s.archiveStatus)
	mux.HandleFunc("/.serve/", http.NotFound)
	if s.uploadRoot != nil {
		mux.HandleFunc("/upload", s.upload)
	}
	mux.HandleFunc("/qr/", s.qr)
	mux.HandleFunc("/qrurl", s.qrurl)
	if s.browserLogging {
		mux.HandleFunc("/log", s.clientLog)
	}
	// instantiateStreaming requires the exact wasm content type.
	mux.HandleFunc("/qrstream.wasm", serveStatic(`application/wasm`, qrWASM))
	mux.HandleFunc("/wasm_exec.js", serveStatic(`text/javascript`, wasmExec))
	mux.HandleFunc("/qrworker.js", serveStatic(`text/javascript`, qrWorker))
	mux.HandleFunc("/style.css", serveStatic(`text/css`, styleCSS))
	return mux
}

func main() {
	browserLogging := flag.Bool(`browser-log`, false, `log browser scanner diagnostics`)
	var uploadLimit byteSizeValue
	flag.Var(&uploadLimit, `upload-limit`, `maximum multipart upload request size, e.g. 500MB or 2GiB (0 is unlimited)`)
	flag.Parse()

	root, err := os.OpenRoot(`.`)
	if err != nil {
		log.Fatal(err)
	}
	defer root.Close()
	srv, err := newServer(root.FS(), root, log.Default(), *browserLogging, int64(uploadLimit))
	if err != nil {
		log.Fatal(err)
	}
	mux := srv.mux()

	ln, err := net.Listen(`tcp`, httpPort)
	if err != nil {
		log.Fatal(err)
	}
	cert, err := serverCert()
	if err != nil {
		log.Fatal(err)
	}
	lnTLS, err := tls.Listen(`tcp`, httpsPort, &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		log.Fatal(err)
	}
	go printURL()
	go func() { log.Fatal(http.Serve(lnTLS, mux)) }()
	if err := http.Serve(ln, mux); err != nil {
		log.Fatal(err)
	}
}

// candidateHosts lists hosts another device might reach, in
// preference order: the default-route IPv4 first, remaining IPv4s,
// then IPv6 (bracketed; link-local excluded - its zone does not
// transfer to another device), localhost last.
func candidateHosts() []string {
	var v4, v6 []string
	add := func(ip net.IP) {
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast() {
			return
		}
		if ip.To4() != nil {
			if s := ip.String(); !slices.Contains(v4, s) {
				v4 = append(v4, s)
			}
		} else if s := `[` + ip.String() + `]`; !slices.Contains(v6, s) {
			v6 = append(v6, s)
		}
	}
	// A UDP connect to TEST-NET-1 asks the kernel which local address
	// its default route would use. No packet is sent because the socket
	// is never written to, and 192.0.2.0/24 is reserved for examples.
	if c, err := net.Dial(`udp`, `192.0.2.1:9`); err == nil {
		add(c.LocalAddr().(*net.UDPAddr).IP)
		c.Close()
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok {
				add(n.IP)
			}
		}
	}
	return append(append(v4, v6...), `localhost`)
}

// alt holds the probe-passed non-loopback hosts, in preference
// order; the loopback QR fallback uses the first one.
var alt struct {
	sync.Mutex
	hosts []string
}

func altHost() string {
	alt.Lock()
	defer alt.Unlock()
	if len(alt.hosts) == 0 {
		return ``
	}
	return alt.hosts[0]
}

// printURL prints the web UI addresses that answer an HTTP probe,
// each non-localhost one with QR codes for both schemes: http scans
// without the self-signed-certificate warning, https is required
// before the browser allows camera access. The probe hits our own
// listener, so it only weeds out locally dead addresses (e.g. a
// downed bridge); which of the remaining addresses another device
// can actually reach is unknowable from here, hence QRs per
// candidate. Survivors are remembered for the loopback QR fallback.
func printURL() {
	hosts := candidateHosts()
	reachable := make([]bool, len(hosts))
	client := &http.Client{Timeout: time.Second}
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Go(func() {
			if resp, err := client.Head(`http://` + h + httpPort + `/`); err == nil {
				resp.Body.Close()
				reachable[i] = true
			}
		})
	}
	wg.Wait()
	for i, h := range hosts {
		if !reachable[i] {
			continue
		}
		if h != `localhost` {
			alt.Lock()
			alt.hosts = append(alt.hosts, h)
			alt.Unlock()
		}
		for _, u := range []string{`http://` + h + httpPort + `/`, `https://` + h + httpsPort + `/`} {
			if h != `localhost` {
				if q, err := qrText(u); err == nil {
					fmt.Print(q)
				}
			}
			fmt.Println(u)
		}
	}
}

// serverCert generates a fresh in-memory self-signed certificate on
// every start and stores nothing: serve runs on borrowed machines
// (a family member's PC, a detached company box), so it must not
// leave key material behind. The per-restart browser warning is the
// accepted cost; one server run typically spans many browser
// sessions.
func serverCert() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: `serve`},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{`localhost`},
	}
	if hn, err := os.Hostname(); err == nil && hn != `` {
		tmpl.DNSNames = append(tmpl.DNSNames, hn)
	}
	for _, h := range candidateHosts() {
		if ip := net.ParseIP(strings.Trim(h, `[]`)); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

// qrText renders content as a single QR code drawn with terminal
// block characters, e.g. to show a URL for scanning off the terminal.
func qrText(content string) (string, error) {
	q, err := qrcode.New(content, qrcode.Medium)
	if err != nil {
		return ``, err
	}
	return q.ToSmallString(false), nil
}

// qrPNG renders content as a single QR code PNG of size×size pixels.
func qrPNG(content string, size int) ([]byte, error) {
	q, err := qrcode.New(content, qrcode.Medium)
	if err != nil {
		return nil, err
	}
	return q.PNG(size)
}

// serveStatic serves an embedded asset, gzip-compressed for clients
// that accept it; compression runs once, on first use.
func serveStatic(contentType string, data []byte) http.HandlerFunc {
	var once sync.Once
	var gz []byte
	return func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			methodNotAllowed(w, http.MethodGet, http.MethodHead)
			return
		}
		w.Header().Set(`Content-Type`, contentType)
		w.Header().Set(`Vary`, `Accept-Encoding`)
		if strings.Contains(req.Header.Get(`Accept-Encoding`), `gzip`) {
			once.Do(func() {
				var buf bytes.Buffer
				zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
				zw.Write(data)
				zw.Close()
				gz = buf.Bytes()
			})
			w.Header().Set(`Content-Encoding`, `gzip`)
			if req.Method == http.MethodHead {
				return
			}
			w.Write(gz)
			return
		}
		if req.Method == http.MethodHead {
			return
		}
		w.Write(data)
	}
}

func methodNotAllowed(w http.ResponseWriter, methods ...string) {
	w.Header().Set(`Allow`, strings.Join(methods, `, `))
	http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
}

// loopbackHost reports whether the request host is localhost or a
// loopback IP. Such a URL must never end up in a QR code: the scan
// happens on another device, where a loopback URL directs the
// scanning device to itself instead of to this host.
func loopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	if host == `localhost` {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, `[]`))
	return ip != nil && ip.IsLoopback()
}

func httpsPageURL(req *http.Request) string {
	host := req.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	} else {
		host = strings.Trim(host, `[]`)
	}
	target := *req.URL
	target.Scheme = `https`
	target.Host = net.JoinHostPort(host, strings.TrimPrefix(httpsPort, `:`))
	return target.String()
}

// qrurl serves a QR code of the URL this request arrived on - the one
// address the connecting browser has proven reachable. For loopback
// requests (where that URL would point the scanning device at
// itself) it falls back to the best probed non-loopback host.
func (s *server) qrurl(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}
	scheme, port := `http`, httpPort
	if req.TLS != nil {
		scheme, port = `https`, httpsPort
	}
	host := req.Host
	if loopbackHost(host) {
		if host = altHost(); host == `` {
			http.NotFound(w, req)
			return
		}
		host += port
	}
	targetPath := req.URL.Query().Get(`path`)
	if !strings.HasPrefix(targetPath, `/`) {
		targetPath = `/`
	}
	target := (&url.URL{Scheme: scheme, Host: host, Path: targetPath}).String()
	png, err := qrPNG(target, 256)
	if err != nil {
		s.requestError(w, req, http.StatusInternalServerError, `could not render URL QR code`, err)
		return
	}
	w.Header().Set(`Content-Type`, `image/png`)
	if req.Method == http.MethodHead {
		return
	}
	w.Write(png)
}

// localName converts an io/fs-style slash-separated name into a local
// path while rejecting absolute paths and dot traversal. The root is
// represented by ".".
func localName(name string) (string, error) {
	if name == `` || name == `.` {
		return `.`, nil
	}
	if !fs.ValidPath(name) {
		return ``, fs.ErrInvalid
	}
	return filepath.Localize(name)
}

// rejectSymlinkDirectories checks every upload destination component.
// A directory symlink is never used for upload placement; os.Root also
// prevents links from escaping the writable tree.
func rejectSymlinkDirectories(root *os.Root, name string) error {
	if name == `.` {
		return nil
	}
	current := `.`
	for component := range strings.SplitSeq(filepath.ToSlash(name), `/`) {
		current = filepath.Join(current, component)
		info, err := root.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		target, err := root.Stat(current)
		if err != nil {
			return err
		}
		if target.IsDir() {
			return fmt.Errorf("%w: %s", filebrowser.ErrSymlinkDirectory, filepath.ToSlash(current))
		}
	}
	return nil
}

func (s *server) openUploadPath(name string) (*os.File, os.FileInfo, error) {
	if err := rejectSymlinkDirectories(s.uploadRoot, name); err != nil {
		return nil, nil, err
	}
	f, err := s.uploadRoot.Open(name)
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

func (s *server) requestError(w http.ResponseWriter, req *http.Request, status int, message string, err error) {
	if err != nil {
		s.logger.Printf("request method=%q path=%q status=%d error=%q", req.Method, req.URL.Path, status, err.Error())
	}
	http.Error(w, message, status)
}

// qr serves the file named after /qr/ as an endless loop of QR codes
// (qrstream format, a multipart motion stream so frames render
// lazily) for camera transfer without any network.
func (s *server) qr(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	name := strings.TrimPrefix(req.URL.Path, `/qr/`)
	if !fs.ValidPath(name) || name == `.` {
		http.NotFound(w, req)
		return
	}
	source, err := payload.OpenFile(s.files, name)
	if err != nil {
		http.NotFound(w, req)
		return
	}
	if size, ok := source.Size(); ok && size > maxQRFileSize {
		s.requestError(w, req, http.StatusRequestEntityTooLarge, `file is too large for QR transfer`, nil)
		return
	}
	data, _, err := payload.ReadAll(req.Context(), source, maxQRFileSize)
	if err != nil {
		if errors.Is(err, payload.ErrTooLarge) {
			s.requestError(w, req, http.StatusRequestEntityTooLarge, `file is too large for QR transfer`, nil)
			return
		}
		s.requestError(w, req, http.StatusInternalServerError, `could not read file`, err)
		return
	}
	// Fountain mode for camera reception: every coded frame reduces
	// the deficit, so there is no single "last frame" the scanner
	// must wait a full loop to catch (sequential mode's
	// coupon-collector tail).
	st, err := qrstream.Encode(source.Filename(), data, &qrstream.Options{Fountain: true})
	if err != nil {
		s.requestError(w, req, http.StatusInternalServerError, `could not encode QR stream`, err)
		return
	}
	st.ServeHTTP(w, req)
}

type listingIcon string

const (
	listingIconFile     listingIcon = `file`
	listingIconFolder   listingIcon = `folder`
	listingIconText     listingIcon = `text`
	listingIconImage    listingIcon = `image`
	listingIconVideo    listingIcon = `video`
	listingIconAudio    listingIcon = `audio`
	listingIconDocument listingIcon = `document`
)

type pageEntry struct {
	Name        string
	Href        string
	QRHref      string
	ArchiveHref string
	Note        string
	Icon        listingIcon
}

type pageData struct {
	Files          []pageEntry
	ArchiveURL     string
	UploadURL      string
	QRURL          string
	WorkerURL      string
	HTTPSURL       string
	ShowQR         bool
	BrowserLogging bool
}

func (s *server) page(w http.ResponseWriter, req *http.Request) {
	s.browser.ServeHTTP(w, req)
}

func (s *server) renderDirectory(w io.Writer, req *http.Request, directory filebrowser.Directory) error {
	entries := make([]pageEntry, 0, len(directory.Entries))
	for _, file := range directory.Entries {
		entry := pageEntry{Name: file.Name, Icon: listingFileIcon(file.Name)}
		switch {
		case file.Err != nil:
			if file.Symlink {
				entry.Icon = listingIconFile
				entry.Note = `unavailable symlink`
			} else {
				entry.Note = `unavailable file`
			}
		case file.IsDir():
			entry.Icon = listingIconFolder
			if file.Symlink {
				entry.Note = `symlink directory not served`
			} else {
				entry.Href = file.Href
				entry.ArchiveHref = archiveURL(file.Path)
			}
		case file.IsRegular():
			entry.Href = file.Href
			entry.QRHref = (&url.URL{Path: `/qr/` + file.Path}).EscapedPath()
		default:
			entry.Note = `special file not served`
		}
		entries = append(entries, entry)
	}
	uploadURL := ``
	if s.uploadRoot != nil {
		uploadURL = `/upload`
		if directory.Name != `.` {
			uploadURL += `?` + url.Values{`dir`: {directory.Name}}.Encode()
		}
	}
	qrURL := `/qrurl?` + url.Values{`path`: {req.URL.Path}}.Encode()
	workerURL := `/qrworker.js`
	if s.browserLogging {
		workerURL += `?log=1`
	}
	data := pageData{
		Files:          entries,
		ArchiveURL:     archiveURL(directory.Name),
		UploadURL:      uploadURL,
		QRURL:          qrURL,
		WorkerURL:      workerURL,
		HTTPSURL:       httpsPageURL(req),
		ShowQR:         !loopbackHost(req.Host) || altHost() != ``,
		BrowserLogging: s.browserLogging,
	}
	return s.template.Execute(w, data)
}

func archiveURL(directory string) string {
	return `/.serve/archive?` + url.Values{`path`: {directory}}.Encode()
}

func listingFileIcon(name string) listingIcon {
	base := strings.ToLower(filepath.Base(name))
	switch base {
	case `makefile`, `dockerfile`, `readme`, `license`, `.gitignore`, `.gitattributes`, `.editorconfig`:
		return listingIconText
	}

	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case `.pdf`, `.doc`, `.docx`, `.odt`, `.ods`, `.odp`, `.rtf`, `.epub`:
		return listingIconDocument
	case `.txt`, `.md`, `.markdown`, `.rst`, `.go`, `.mod`, `.sum`, `.c`, `.h`, `.cc`, `.cpp`, `.hpp`, `.rs`, `.py`, `.rb`, `.sh`, `.js`, `.mjs`, `.cjs`, `.ts`, `.tsx`, `.jsx`, `.css`, `.scss`, `.html`, `.htm`, `.xml`, `.json`, `.yaml`, `.yml`, `.toml`, `.ini`, `.conf`, `.log`, `.csv`, `.tsv`, `.sql`:
		return listingIconText
	case `.png`, `.jpg`, `.jpeg`, `.gif`, `.webp`, `.svg`, `.avif`, `.bmp`, `.tif`, `.tiff`, `.heic`:
		return listingIconImage
	case `.mp4`, `.m4v`, `.mov`, `.mkv`, `.webm`, `.avi`, `.mpeg`, `.mpg`:
		return listingIconVideo
	case `.mp3`, `.m4a`, `.aac`, `.flac`, `.ogg`, `.oga`, `.opus`, `.wav`:
		return listingIconAudio
	}

	contentType := mime.TypeByExtension(ext)
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return listingIconFile
	}
	switch {
	case strings.HasPrefix(mediaType, `image/`):
		return listingIconImage
	case strings.HasPrefix(mediaType, `video/`):
		return listingIconVideo
	case strings.HasPrefix(mediaType, `audio/`):
		return listingIconAudio
	case filebrowser.IsTextMediaType(mediaType):
		return listingIconText
	default:
		return listingIconFile
	}
}

func sameOrigin(req *http.Request) bool {
	origin := req.Header.Get(`Origin`)
	if origin == `` {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == `` {
		return false
	}
	scheme := `http`
	if req.TLS != nil {
		scheme = `https`
	}
	return u.Scheme == scheme && u.Host == req.Host
}

func (s *server) clientLog(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if !sameOrigin(req) {
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, maxClientLog))
	if err != nil {
		status := http.StatusBadRequest
		message := `could not read browser log`
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			status = http.StatusRequestEntityTooLarge
			message = `browser log is too large`
		}
		s.requestError(w, req, status, message, err)
		return
	}
	if message := strings.TrimSpace(string(body)); message != `` {
		s.logger.Printf("browser message=%q", message)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) upload(w http.ResponseWriter, req *http.Request) {
	if s.uploadRoot == nil {
		http.NotFound(w, req)
		return
	}
	if req.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if !sameOrigin(req) {
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		return
	}
	if s.uploadLimit > 0 {
		if req.ContentLength > s.uploadLimit {
			err := fmt.Errorf("request body is %d bytes; upload limit is %d", req.ContentLength, s.uploadLimit)
			s.requestError(w, req, http.StatusRequestEntityTooLarge, `upload exceeds configured limit`, err)
			return
		}
		req.Body = http.MaxBytesReader(w, req.Body, s.uploadLimit)
	}
	dir, err := localName(req.URL.Query().Get(`dir`))
	if err != nil {
		s.requestError(w, req, http.StatusBadRequest, `invalid upload directory`, err)
		return
	}
	d, info, err := s.openUploadPath(dir)
	if err != nil {
		http.NotFound(w, req)
		return
	}
	d.Close()
	if !info.IsDir() {
		s.requestError(w, req, http.StatusBadRequest, `upload destination is not a directory`, nil)
		return
	}
	mr, err := req.MultipartReader()
	if err != nil {
		s.requestError(w, req, http.StatusBadRequest, `expected a multipart upload`, err)
		return
	}
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			status := http.StatusBadRequest
			message := `could not read multipart upload`
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				status = http.StatusRequestEntityTooLarge
				message = `upload exceeds configured limit`
			}
			s.requestError(w, req, status, message, err)
			return
		}
		name := p.FileName()
		if name == `` {
			p.Close()
			continue
		}
		written, err := storeUpload(s.uploadRoot, dir, name, p)
		p.Close()
		if err != nil {
			status := http.StatusInternalServerError
			message := `could not store upload`
			var maxErr *http.MaxBytesError
			var inputErr *uploadInputError
			switch {
			case errors.As(err, &maxErr):
				status = http.StatusRequestEntityTooLarge
				message = `upload exceeds configured limit`
			case errors.Is(err, fs.ErrInvalid):
				status = http.StatusBadRequest
				message = `invalid upload filename`
			case errors.As(err, &inputErr):
				status = http.StatusBadRequest
				message = `upload was interrupted`
			}
			s.requestError(w, req, status, message, err)
			return
		}
		s.logger.Printf("upload path=%q bytes=%d", filepath.ToSlash(filepath.Join(dir, name)), written)
	}
	w.WriteHeader(http.StatusNoContent)
}

type uploadInputError struct {
	err error
}

func (e *uploadInputError) Error() string { return e.err.Error() }
func (e *uploadInputError) Unwrap() error { return e.err }

type uploadPartReader struct {
	part *multipart.Part
}

func (r uploadPartReader) Read(p []byte) (int, error) {
	n, err := r.part.Read(p)
	if err != nil && err != io.EOF {
		err = &uploadInputError{err: err}
	}
	return n, err
}

func storeUpload(root *os.Root, dir, name string, src *multipart.Part) (int64, error) {
	if name == `.` || !filepath.IsLocal(name) {
		return 0, fs.ErrInvalid
	}
	target := filepath.Join(dir, name)
	temp, tempName, err := createUploadTemp(root, dir)
	if err != nil {
		return 0, err
	}
	keep := false
	defer func() {
		temp.Close()
		if !keep {
			root.Remove(tempName)
		}
	}()
	written, err := io.Copy(temp, uploadPartReader{part: src})
	if err != nil {
		return 0, err
	}
	if err := temp.Chmod(0o644); err != nil {
		return 0, err
	}
	if err := temp.Close(); err != nil {
		return 0, err
	}
	// Replacement is intentional. The temporary file keeps an aborted
	// upload from leaving a partially written destination.
	if err := root.Rename(tempName, target); err != nil {
		return 0, err
	}
	keep = true
	return written, nil
}

func createUploadTemp(root *os.Root, dir string) (*os.File, string, error) {
	for range 100 {
		var random [8]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, ``, err
		}
		name := filepath.Join(dir, `.serve-upload-`+hex.EncodeToString(random[:]))
		f, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return f, name, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, ``, err
		}
	}
	return nil, ``, errors.New(`could not allocate upload temporary file`)
}
