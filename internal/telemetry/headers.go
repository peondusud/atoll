package telemetry

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/peondusud/atoll/internal/fsx"
)

// maxHeadersFile is the most a file of headers may hold.
const maxHeadersFile = 16 << 10

// errHeaders says that a file of headers cannot be used. Its errors never hold what the file holds: a token.
var errHeaders = errors.New("the OTLP headers file")

// header is one header of the file.
type header struct {
	name, value string
}

// reserved are the headers the exporter sets, which a file may not change.
var reserved = []string{"Content-Type", "Content-Length", "Content-Encoding", "Transfer-Encoding", "Host", "Connection"}

// readHeaders reads a file of headers: one Name: value a line, an empty line or one that starts with # aside. A name is
// an HTTP token, a value has no control character but a tab, and neither is ever in an error.
func readHeaders(file string) ([]header, error) {
	// Read with its limit, never whole first: a file of headers is a line or a few.
	data, err := fsx.ReadSmallFile(file, maxHeadersFile)
	if err != nil {
		return nil, fmt.Errorf("%w %s: %w", errHeaders, file, err)
	}
	var out []header
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if trimmed := strings.TrimSpace(line); trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		name, value = strings.TrimSpace(name), strings.Trim(value, " \t")
		switch {
		case !ok || !token(name):
			return nil, fmt.Errorf("%w %s: line %d: no header name, then a colon", errHeaders, file, i+1)
		case !fieldValue(value):
			return nil, fmt.Errorf("%w %s: line %d: a control character in the value of %s", errHeaders, file, i+1, name)
		}
		if canonical := http.CanonicalHeaderKey(name); slices.Contains(reserved, canonical) {
			return nil, fmt.Errorf("%w %s: line %d: %s is the exporter's", errHeaders, file, i+1, canonical)
		}
		out = append(out, header{name: name, value: value})
	}
	return out, nil
}

// token says whether a name is an HTTP token (RFC 9110).
func token(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		alnum := 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9'
		if !alnum && !strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c)) {
			return false
		}
	}
	return true
}

// fieldValue says whether a value holds no control character but a tab.
func fieldValue(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 && c != '\t' || c == 0x7f {
			return false
		}
	}
	return true
}

// withHeaders adds the headers of a file to each request, read again for each, so that a new token takes effect at
// once (specs.md §23.2).
type withHeaders struct {
	base http.RoundTripper
	file string
}

func (t *withHeaders) RoundTrip(req *http.Request) (*http.Response, error) {
	headers, err := readHeaders(t.file)
	if err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	out := req.Clone(req.Context())
	for _, h := range headers {
		out.Header.Add(h.name, h.value)
	}
	return t.base.RoundTrip(out)
}

// newClient gives the client both exporters send with: the system's roots for https://, the proxy of the environment,
// timeout for a request, its connection included, a redirect never followed, as the headers would go wherever it
// leads, and the headers of the file when one is set.
func newClient(timeout time.Duration, headersFile string) *http.Client {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          2,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		ExpectContinueTimeout: time.Second,
	}
	var rt http.RoundTripper = transport
	if headersFile != "" {
		rt = &withHeaders{base: transport, file: headersFile}
	}
	return &http.Client{
		Transport:     rt,
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}
