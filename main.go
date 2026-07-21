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
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"image"
	"image/png"
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
	"strconv"
	"strings"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"
	"github.com/srlehn/serve/filebrowser"
	"github.com/srlehn/serve/internal/payload"
	"github.com/srlehn/serve/internal/unixarchive"
	"github.com/srlehn/serve/jabstream"
	"github.com/srlehn/serve/qrstream"
)

const (
	httpPort           = `:8000`
	httpsPort          = `:8443`
	maxBarcodeFileSize = 64 << 20
	maxClientLog       = 16 << 10
	// maxStoredFramePixels bounds one camera frame posted to the
	// frame store, mirroring the wasm shim's allocation bound.
	maxStoredFramePixels = 4096 * 4096
	frameStorePath       = `/framestore`
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

// the camera scanners: the qrstream and jabstream decoders compiled
// to wasm plus the shared JS loader. Rebuild with go generate.
//
//go:generate go run wasm/generate.go
var (
	//go:embed wasm/qrstream.wasm
	qrWASM []byte
	//go:embed wasm/jabstream.wasm
	jabWASM []byte
	//go:embed wasm/wasm_exec.js
	wasmExec []byte
	//go:embed wasm/qrworker.js
	qrWorker []byte
	//go:embed wasm/jabworker.js
	jabWorker []byte
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
	frameStore      *frameStore
	archiveStatuses archiveStatusStore
}

func newServer(files fs.FS, uploadRoot *os.Root, logger *log.Logger, browserLogging bool, uploadLimit int64, frameStorePrefix string) (*server, error) {
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
	if frameStorePrefix != `` {
		s.frameStore, err = newFrameStore(frameStorePrefix)
		if err != nil {
			return nil, err
		}
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
	mux.HandleFunc("/jab/", s.jab)
	mux.HandleFunc("/qrurl", s.qrurl)
	if s.browserLogging {
		mux.HandleFunc("/log", s.clientLog)
	}
	if s.frameStore != nil {
		mux.HandleFunc(frameStorePath, s.storeFrame)
	}
	// instantiateStreaming requires the exact wasm content type.
	mux.HandleFunc("/qrstream.wasm", serveStatic(`application/wasm`, qrWASM))
	mux.HandleFunc("/jabstream.wasm", serveStatic(`application/wasm`, jabWASM))
	mux.HandleFunc("/wasm_exec.js", serveStatic(`text/javascript`, wasmExec))
	mux.HandleFunc("/qrworker.js", serveStatic(`text/javascript`, qrWorker))
	mux.HandleFunc("/jabworker.js", serveStatic(`text/javascript`, jabWorker))
	mux.HandleFunc("/style.css", serveStatic(`text/css`, styleCSS))
	return mux
}

func main() {
	browserLogging := flag.Bool(`browser-log`, false, `log browser scanner diagnostics`)
	storeFrames := flag.String(`store-frames`, ``, `store scanned camera frames as PNGs with this filename prefix, plus a metadata log (trailing / stores into a directory)`)
	var uploadLimit byteSizeValue
	flag.Var(&uploadLimit, `upload-limit`, `maximum multipart upload request size, e.g. 500MB or 2GiB (0 is unlimited)`)
	flag.Parse()

	root, err := os.OpenRoot(`.`)
	if err != nil {
		log.Fatal(err)
	}
	defer root.Close()
	srv, err := newServer(root.FS(), root, log.Default(), *browserLogging, int64(uploadLimit), *storeFrames)
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

// hostClass ranks a candidate host by the kind of interface it lives
// on. The values are the page QR's preference order: a phone scanning
// the code shares the Wi-Fi first, a wired LAN next, a VPN overlay it
// might have joined last. A host-local virtual bridge (libvirt,
// Docker) is never a phone's network and is kept out of the page QR.
type hostClass int

const (
	classWireless hostClass = iota
	classWired
	classTunnel
	classVirtual
)

// hostCandidate is a host another device might reach, labeled with
// its network interface and classed by interface kind so the page QR
// can prefer the network a scanning phone shares.
type hostCandidate struct {
	host  string // formatted for URLs: IPv6 bracketed
	iface string // empty for localhost and the label-less fallback
	class hostClass
}

// tunnelInterface reports whether an interface carries VPN or tunnel
// traffic rather than an ordinary LAN. The point-to-point flag covers
// tun-style VPNs such as Tailscale and WireGuard; the name prefixes
// catch tap-style and userspace tunnels that do not set it.
func tunnelInterface(name string, flags net.Flags) bool {
	if flags&net.FlagPointToPoint != 0 {
		return true
	}
	for _, prefix := range []string{`tun`, `tap`, `utun`, `wg`, `tailscale`, `zt`, `ppp`} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// virtualInterface reports whether an interface is a host-local
// virtual bridge or VM link (libvirt, Docker, VirtualBox, VMware). No
// external device shares such a network, so its addresses stay out of
// the page QR. The "br-" prefix is Docker's bridge naming; a bare
// "br0" bridging a physical LAN is deliberately not matched.
func virtualInterface(name string) bool {
	for _, prefix := range []string{`virbr`, `docker`, `br-`, `veth`, `vnet`, `vboxnet`, `vmnet`} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// wirelessInterface reports whether an interface is Wi-Fi. Linux marks
// wireless devices in sysfs (a "wireless" attribute directory and a
// "phy80211" link); elsewhere, and as a fallback, conventional name
// prefixes are used.
func wirelessInterface(name string) bool {
	for _, marker := range []string{`phy80211`, `wireless`} {
		if _, err := os.Stat(`/sys/class/net/` + name + `/` + marker); err == nil {
			return true
		}
	}
	for _, prefix := range []string{`wl`, `wifi`, `ath`, `ra`, `iwn`, `iwm`} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// interfaceClass classifies an interface for the page QR preference
// order. Virtual bridges are checked first so a Docker or libvirt
// bridge is never mistaken for a LAN, then tunnels, then Wi-Fi;
// anything else is treated as a wired LAN.
func interfaceClass(name string, flags net.Flags) hostClass {
	switch {
	case virtualInterface(name):
		return classVirtual
	case tunnelInterface(name, flags):
		return classTunnel
	case wirelessInterface(name):
		return classWireless
	default:
		return classWired
	}
}

// ifaceAddrs is one up interface with its addresses and class,
// decoupled from net.Interfaces so candidate collection is testable.
type ifaceAddrs struct {
	name  string
	class hostClass
	ips   []net.IP
}

func localIfaceAddrs() []ifaceAddrs {
	ifaces, err := net.Interfaces()
	if err != nil {
		// Fall back to unlabeled wired addresses rather than none.
		var ips []net.IP
		if addrs, err := net.InterfaceAddrs(); err == nil {
			for _, a := range addrs {
				if n, ok := a.(*net.IPNet); ok {
					ips = append(ips, n.IP)
				}
			}
		}
		return []ifaceAddrs{{class: classWired, ips: ips}}
	}
	var out []ifaceAddrs
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		ia := ifaceAddrs{name: ifc.Name, class: interfaceClass(ifc.Name, ifc.Flags)}
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok {
				ia.ips = append(ia.ips, n.IP)
			}
		}
		out = append(out, ia)
	}
	return out
}

// defaultRouteIP asks the kernel which local address its default
// route would use, via a UDP connect to TEST-NET-1. No packet is sent
// because the socket is never written to, and 192.0.2.0/24 is
// reserved for examples.
func defaultRouteIP() net.IP {
	c, err := net.Dial(`udp`, `192.0.2.1:9`)
	if err != nil {
		return nil
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP
}

// candidateHosts lists hosts another device might reach, in the page
// QR's preference order: Wi-Fi first, then wired LAN, then VPN or
// tunnel, then host-local virtual bridges, with IPv4 before IPv6 and
// the default-route address leading its group (bracketed for IPv6;
// link-local excluded - its zone does not transfer to another
// device); localhost last.
func candidateHosts() []hostCandidate {
	return collectHostCandidates(defaultRouteIP(), localIfaceAddrs())
}

func collectHostCandidates(defaultRoute net.IP, ifaces []ifaceAddrs) []hostCandidate {
	// Groups run wireless, wired, tunnel, virtual - the page QR's
	// preference order - each split IPv4 before IPv6. Virtual bridge
	// addresses are collected for the terminal listing and the
	// certificate but dropped from the page QR pool by printURL.
	var groups [8][]hostCandidate
	seen := make(map[string]bool)
	for _, ifc := range ifaces {
		for _, ip := range ifc.ips {
			if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast() {
				continue
			}
			host, ver := ip.String(), 0
			if ip.To4() == nil {
				host, ver = `[`+host+`]`, 1
			}
			if seen[host] {
				continue
			}
			seen[host] = true
			g := int(ifc.class)*2 + ver
			c := hostCandidate{host: host, iface: ifc.name, class: ifc.class}
			if ip.Equal(defaultRoute) {
				groups[g] = append([]hostCandidate{c}, groups[g]...)
			} else {
				groups[g] = append(groups[g], c)
			}
		}
	}
	out := slices.Concat(groups[0], groups[1], groups[2], groups[3], groups[4], groups[5], groups[6], groups[7])
	return append(out, hostCandidate{host: `localhost`})
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
// each phone-reachable one with QR codes for both schemes: http scans
// without the self-signed-certificate warning, https is required
// before the browser allows camera access. localhost and host-local
// virtual bridges print as plain URLs with no QR: no scanning phone
// can reach them. The probe hits our own listener, so it only weeds
// out locally dead addresses (e.g. a downed bridge); which of the
// remaining addresses another device can actually reach is unknowable
// from here, hence QRs per candidate. Survivors are remembered for the
// loopback QR fallback.
//
// The barcoded candidates are collected best first but printed worst
// first, so the top-preference codes are the ones left on screen at
// the cursor. localhost is pinned last of all: it carries no barcode,
// so its two short lines stay at the cursor for running locally
// without pushing the good codes off screen.
func printURL() {
	hosts := candidateHosts()
	reachable := make([]bool, len(hosts))
	client := &http.Client{Timeout: time.Second}
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Go(func() {
			if resp, err := client.Head(`http://` + h.host + httpPort + `/`); err == nil {
				resp.Body.Close()
				reachable[i] = true
			}
		})
	}
	wg.Wait()
	// Remember the reachable non-virtual hosts for the loopback QR
	// fallback in preference order, best first.
	alt.Lock()
	for i, h := range hosts {
		if reachable[i] && h.host != `localhost` && h.class != classVirtual {
			alt.hosts = append(alt.hosts, h.host)
		}
	}
	alt.Unlock()
	printHost := func(h hostCandidate) {
		for _, u := range []string{`http://` + h.host + httpPort + `/`, `https://` + h.host + httpsPort + `/`} {
			if h.host != `localhost` && h.class != classVirtual {
				if q, err := qrText(u); err == nil {
					fmt.Print(q)
				}
			}
			fmt.Println(u + hostLabel(h))
		}
	}
	// Barcoded hosts worst first, then localhost last so its short,
	// barcode-less lines stay at the cursor.
	for i, h := range slices.Backward(hosts) {
		if reachable[i] && h.host != `localhost` {
			printHost(h)
		}
	}
	for i, h := range hosts {
		if reachable[i] && h.host == `localhost` {
			printHost(h)
		}
	}
}

// hostLabel renders the interface suffix printed after a URL, e.g.
// " (eth0)", " (tailscale0, VPN/tunnel)", or " (virbr0, virtual)".
func hostLabel(h hostCandidate) string {
	if h.iface == `` {
		return ``
	}
	label := ` (` + h.iface
	switch h.class {
	case classTunnel:
		label += `, VPN/tunnel`
	case classVirtual:
		label += `, virtual`
	}
	return label + `)`
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
		if ip := net.ParseIP(strings.Trim(h.host, `[]`)); ip != nil {
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

// barcodeSource resolves the transfer target of a barcode sender: a
// regular file streams as-is, a directory as its compressed Unix
// archive. It writes the per-request error response itself when it
// fails. Symlinked directories stay rejected exactly as in the file
// tree.
func (s *server) barcodeSource(w http.ResponseWriter, req *http.Request, prefix string) (payload.Source, bool) {
	name := strings.TrimPrefix(req.URL.Path, prefix)
	if !fs.ValidPath(name) {
		http.NotFound(w, req)
		return nil, false
	}
	file, info, err := filebrowser.Open(s.files, name)
	if err != nil {
		http.NotFound(w, req)
		return nil, false
	}
	file.Close()
	if info.IsDir() {
		source, err := unixarchive.New(s.files, name, unixarchive.Options{
			RootName: s.archiveRootName(name),
			OSRoot:   s.archiveRoot,
			Warning: func(warning payload.Warning) {
				s.logger.Printf("barcode archive path=%q member=%q warning=%q", name, warning.Path, warning.Message)
			},
		})
		if err != nil {
			http.NotFound(w, req)
			return nil, false
		}
		return source, true
	}
	source, err := payload.OpenFile(s.files, name)
	if err != nil {
		http.NotFound(w, req)
		return nil, false
	}
	return source, true
}

// barcodePayload produces the bounded in-memory payload a barcode
// stream encodes, writing the per-request error response itself when
// it fails. label names the transfer in error messages.
func (s *server) barcodePayload(w http.ResponseWriter, req *http.Request, source payload.Source, label string) ([]byte, bool) {
	if size, ok := source.Size(); ok && size > maxBarcodeFileSize {
		s.requestError(w, req, http.StatusRequestEntityTooLarge, `file is too large for `+label+` transfer`, nil)
		return nil, false
	}
	data, report, err := payload.ReadAll(req.Context(), source, maxBarcodeFileSize)
	if err != nil {
		if errors.Is(err, payload.ErrTooLarge) {
			s.requestError(w, req, http.StatusRequestEntityTooLarge, `file is too large for `+label+` transfer`, nil)
			return nil, false
		}
		s.requestError(w, req, http.StatusInternalServerError, `could not read file`, err)
		return nil, false
	}
	if warnings := report.WarningCount(); warnings > 0 {
		s.logger.Printf("barcode payload path=%q warnings=%d", req.URL.Path, warnings)
	}
	return data, true
}

// qr serves the file or directory archive named after /qr/ as an
// endless loop of QR codes (qrstream format, a multipart motion
// stream so frames render lazily) for camera transfer without any
// network.
func (s *server) qr(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	source, ok := s.barcodeSource(w, req, `/qr/`)
	if !ok {
		return
	}
	data, ok := s.barcodePayload(w, req, source, `QR`)
	if !ok {
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

// jab serves the file or directory archive named after /jab/ as an
// endless loop of JAB Code symbols, the higher-density color peer of
// the QR sender with the same per-request guarantees. An optional
// colors query selects the module color count from the modes this
// build supports (16 and 32 are experimental high-color profiles;
// on camera captures 16 reads reliably and 32 marginally).
func (s *server) jab(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	var plan jabstream.Plan
	if value := req.URL.Query().Get(`colors`); value != `` {
		colors, err := strconv.Atoi(value)
		if err != nil || !slices.Contains(jabstream.SupportedColors(), colors) {
			supported := fmt.Sprint(jabstream.SupportedColors())
			s.requestError(w, req, http.StatusBadRequest,
				`unsupported color count; this build supports `+supported, err)
			return
		}
		plan.Colors = colors
	}
	source, ok := s.barcodeSource(w, req, `/jab/`)
	if !ok {
		return
	}
	data, ok := s.barcodePayload(w, req, source, `JAB Code`)
	if !ok {
		return
	}
	st, err := jabstream.Encode(source.Filename(), data, &jabstream.Options{Plan: plan, Fountain: true})
	if err != nil {
		s.requestError(w, req, http.StatusInternalServerError, `could not encode JAB Code stream`, err)
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
	ArchiveHref string
	Actions     []pageAction
	Note        string
	Icon        listingIcon
}

type pageAction struct {
	BackendID      string
	CSSClass       string
	Href           string
	Title          string
	AccessibleName string
	IconID         string
}

type scannerBackend struct {
	ID string
	// IconID names the sprite symbol used both for this backend's
	// listing transfer action and for its button in the camera
	// symbology chooser, so the two share one drawing.
	IconID        string
	WorkerURL     string
	DialogTitle   string
	IdlePrompt    string
	TransferLabel string
	// BurstFrames is how many camera frames the page captures in
	// quick succession before decoding them as one sequence. A
	// decoder that accumulates evidence across frames of one symbol
	// (jabcode Stream) needs several captures of the same displayed
	// frame; a single-shot decoder wants 1.
	BurstFrames int
}

func qrScannerBackend(browserLogging bool) scannerBackend {
	workerURL := `/qrworker.js`
	if browserLogging {
		workerURL += `?log=1`
	}
	return scannerBackend{
		ID:            `qr`,
		IconID:        `action-icon-qr`,
		WorkerURL:     workerURL,
		DialogTitle:   `receive QR stream`,
		IdlePrompt:    `point the camera at the QR codes`,
		TransferLabel: `QR`,
		BurstFrames:   1,
	}
}

func jabScannerBackend(browserLogging bool) scannerBackend {
	workerURL := `/jabworker.js`
	if browserLogging {
		workerURL += `?log=1`
	}
	return scannerBackend{
		ID:            `jab`,
		IconID:        `action-icon-jab`,
		WorkerURL:     workerURL,
		DialogTitle:   `receive JAB Code stream`,
		IdlePrompt:    `point the camera at the JAB Code stream`,
		TransferLabel: `JAB Code`,
		BurstFrames:   4,
	}
}

type transferBackend struct {
	ID               string
	SenderAvailable  bool
	ScannerAvailable bool
	SenderPathPrefix string
	WasmURL          string
	ActionLabel      string
	ActionClass      string
	ActionIconID     string
	DirActionIconID  string
	Scanner          scannerBackend
}

func configuredTransferBackends(browserLogging bool) []transferBackend {
	return []transferBackend{
		{
			ID:               `qr`,
			SenderAvailable:  true,
			ScannerAvailable: true,
			SenderPathPrefix: `/qr/`,
			WasmURL:          `/qrstream.wasm`,
			ActionLabel:      `QR codes`,
			ActionClass:      `qr-transfer`,
			ActionIconID:     `action-icon-qr`,
			DirActionIconID:  `action-icon-qr-archive`,
			Scanner:          qrScannerBackend(browserLogging),
		},
		{
			ID:               `jab`,
			SenderAvailable:  true,
			ScannerAvailable: true,
			SenderPathPrefix: `/jab/`,
			WasmURL:          `/jabstream.wasm`,
			ActionLabel:      `JAB Code`,
			ActionClass:      `jab-transfer`,
			ActionIconID:     `action-icon-jab`,
			DirActionIconID:  `action-icon-jab-archive`,
			Scanner:          jabScannerBackend(browserLogging),
		},
	}
}

// availableScanners lists the compiled-in scanner descriptors, in
// backend order; the first one is the initial selection.
func availableScanners(backends []transferBackend) []scannerBackend {
	var scanners []scannerBackend
	for _, backend := range backends {
		if backend.ScannerAvailable {
			scanners = append(scanners, backend.Scanner)
		}
	}
	return scanners
}

// transferActions builds the per-entry sender actions. A directory
// action transfers the entry as a compressed Unix archive and gets a
// combined icon so it is not mistaken for a plain file transfer.
func transferActions(backends []transferBackend, name, path string, directory bool) []pageAction {
	actions := make([]pageAction, 0, len(backends))
	for _, backend := range backends {
		if !backend.SenderAvailable {
			continue
		}
		action := pageAction{
			BackendID:      backend.ID,
			CSSClass:       backend.ActionClass,
			Href:           (&url.URL{Path: backend.SenderPathPrefix + path}).EscapedPath(),
			Title:          `transfer via ` + backend.ActionLabel,
			AccessibleName: `transfer ` + name + ` via ` + backend.ActionLabel,
			IconID:         backend.ActionIconID,
		}
		if directory {
			action.Title = `transfer as compressed archive via ` + backend.ActionLabel
			action.AccessibleName = `transfer ` + name + ` as a compressed archive via ` + backend.ActionLabel
			action.IconID = backend.DirActionIconID
		}
		actions = append(actions, action)
	}
	return actions
}

type pageData struct {
	Files          []pageEntry
	Backends       []transferBackend
	ArchiveURL     string
	DirActions     []pageAction
	UploadURL      string
	QRURL          string
	Scanner        scannerBackend   // initial selection
	Scanners       []scannerBackend // every compiled-in scanner
	HTTPSURL       string
	ShowQR         bool
	BrowserLogging bool
	FrameStoreURL  string // empty unless -store-frames is set
}

func (s *server) page(w http.ResponseWriter, req *http.Request) {
	s.browser.ServeHTTP(w, req)
}

func (s *server) renderDirectory(w io.Writer, req *http.Request, directory filebrowser.Directory) error {
	backends := configuredTransferBackends(s.browserLogging)
	scanners := availableScanners(backends)
	if len(scanners) == 0 {
		return errors.New(`no scanner backend is available`)
	}
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
				entry.Actions = transferActions(backends, file.Name, file.Path, true)
			}
		case file.IsRegular():
			entry.Href = file.Href
			entry.Actions = transferActions(backends, file.Name, file.Path, false)
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
	currentName := directory.Name
	if currentName == `.` {
		currentName = `this directory`
	}
	frameStoreURL := ``
	if s.frameStore != nil {
		frameStoreURL = frameStorePath
	}
	data := pageData{
		Files:          entries,
		Backends:       backends,
		ArchiveURL:     archiveURL(directory.Name),
		DirActions:     transferActions(backends, currentName, directory.Name, true),
		UploadURL:      uploadURL,
		QRURL:          qrURL,
		Scanner:        scanners[0],
		Scanners:       scanners,
		HTTPSURL:       httpsPageURL(req),
		ShowQR:         !loopbackHost(req.Host) || altHost() != ``,
		BrowserLogging: s.browserLogging,
		FrameStoreURL:  frameStoreURL,
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

// frameStore writes camera frames posted by the scanner page to
// disk: one lossless PNG per frame named prefix<start>-<n>.png plus
// one JSON metadata line each in prefixframes.jsonl. The per-process
// start stamp keeps repeated runs with the same prefix from
// overwriting each other. It exists for field debugging: a failing
// scan leaves the exact decoder input behind as replayable fixtures.
type frameStore struct {
	prefix string
	start  string
	mu     sync.Mutex
	n      int
}

func newFrameStore(prefix string) (*frameStore, error) {
	if dir := filepath.Dir(prefix); dir != `.` {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf(`frame store: %w`, err)
		}
	}
	return &frameStore{prefix: prefix, start: time.Now().Format(`20060102-150405`)}, nil
}

// frameMeta is the metadata stored with each frame: the page reports
// capture geometry, the server adds provenance. The file name is
// relative to the metadata log's own directory.
type frameMeta struct {
	File string `json:"file"`
	Time string `json:"time"`
	// Seq numbers every page-side capture, stored or skipped, so
	// gaps in the stored sequence are visible; Dropped is the
	// running count of frames the page skipped under upload
	// backpressure.
	Seq         int64   `json:"seq,omitempty"`
	Dropped     int64   `json:"dropped,omitempty"`
	Width       int     `json:"width"`
	Height      int     `json:"height"`
	VideoWidth  int     `json:"video_width,omitempty"`
	VideoHeight int     `json:"video_height,omitempty"`
	Zoom        float64 `json:"zoom,omitempty"`
	CropZoom    float64 `json:"crop_zoom,omitempty"`
	Backend     string  `json:"backend,omitempty"`
	PageTimeMS  int64   `json:"page_time_ms,omitempty"`
	UserAgent   string  `json:"user_agent,omitempty"`
	RemoteAddr  string  `json:"remote_addr,omitempty"`
}

func (st *frameStore) store(img *image.NRGBA, meta frameMeta) (string, error) {
	var buf bytes.Buffer
	// BestSpeed: the store keeps up with a scan session; PNG stays
	// lossless at every compression level.
	if err := (&png.Encoder{CompressionLevel: png.BestSpeed}).Encode(&buf, img); err != nil {
		return ``, err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.n++
	name := fmt.Sprintf(`%s%s-%04d.png`, st.prefix, st.start, st.n)
	if err := os.WriteFile(name, buf.Bytes(), 0o644); err != nil {
		return ``, err
	}
	meta.File = filepath.Base(name)
	line, err := json.Marshal(meta)
	if err != nil {
		return ``, err
	}
	f, err := os.OpenFile(st.prefix+`frames.jsonl`, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return ``, err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return ``, err
	}
	return name, f.Close()
}

// storeFrame receives one camera frame from the scanner page as raw
// RGBA bytes (the exact ImageData the decoder worker gets) and hands
// it to the frame store. Registered only when -store-frames is set.
func (s *server) storeFrame(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if !sameOrigin(req) {
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		return
	}
	query := req.URL.Query()
	width, _ := strconv.Atoi(query.Get(`w`))
	height, _ := strconv.Atoi(query.Get(`h`))
	if width <= 0 || height <= 0 || width > maxStoredFramePixels/height {
		s.requestError(w, req, http.StatusBadRequest,
			fmt.Sprintf(`bad frame dimensions %dx%d`, width, height), nil)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, 4*maxStoredFramePixels))
	if err != nil {
		status := http.StatusBadRequest
		message := `could not read the frame`
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			status = http.StatusRequestEntityTooLarge
			message = `frame is too large`
		}
		s.requestError(w, req, status, message, err)
		return
	}
	if len(body) != 4*width*height {
		s.requestError(w, req, http.StatusBadRequest,
			fmt.Sprintf(`frame has %d bytes, dimensions %dx%d need %d`, len(body), width, height, 4*width*height), nil)
		return
	}
	img := &image.NRGBA{Pix: body, Stride: 4 * width, Rect: image.Rect(0, 0, width, height)}
	videoWidth, _ := strconv.Atoi(query.Get(`vw`))
	videoHeight, _ := strconv.Atoi(query.Get(`vh`))
	zoom, _ := strconv.ParseFloat(query.Get(`zoom`), 64)
	cropZoom, _ := strconv.ParseFloat(query.Get(`crop`), 64)
	pageTime, _ := strconv.ParseInt(query.Get(`t`), 10, 64)
	seq, _ := strconv.ParseInt(query.Get(`seq`), 10, 64)
	dropped, _ := strconv.ParseInt(query.Get(`dropped`), 10, 64)
	name, err := s.frameStore.store(img, frameMeta{
		Time:        time.Now().Format(time.RFC3339Nano),
		Seq:         seq,
		Dropped:     dropped,
		Width:       width,
		Height:      height,
		VideoWidth:  videoWidth,
		VideoHeight: videoHeight,
		Zoom:        zoom,
		CropZoom:    cropZoom,
		Backend:     query.Get(`backend`),
		PageTimeMS:  pageTime,
		UserAgent:   req.UserAgent(),
		RemoteAddr:  req.RemoteAddr,
	})
	if err != nil {
		s.requestError(w, req, http.StatusInternalServerError, `could not store the frame`, err)
		return
	}
	s.logger.Printf(`stored frame %s seq=%d dropped=%d`, name, seq, dropped)
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
