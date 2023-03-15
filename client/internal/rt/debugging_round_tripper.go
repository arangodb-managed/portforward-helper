package rt

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// debuggingRoundTripper will display information about the requests passing
// through it based on what is configured
type debuggingRoundTripper struct {
	delegatedRoundTripper http.RoundTripper
	levels                map[DebugLevel]bool
	logger                zerolog.Logger
}

// DebugLevel is used to enable debugging of certain
// HTTP requests and responses fields via the debuggingRoundTripper.
type DebugLevel int

const (
	// DebugJustURL will add to the debug output HTTP requests method and url.
	DebugJustURL DebugLevel = iota
	// DebugURLTiming will add to the debug output the duration of HTTP requests.
	DebugURLTiming
	// DebugCurlCommand will add to the debug output the curl command equivalent to the
	// HTTP request.
	DebugCurlCommand
	// DebugRequestHeaders will add to the debug output the HTTP requests headers.
	DebugRequestHeaders
	// DebugResponseStatus will add to the debug output the HTTP response status.
	DebugResponseStatus
	// DebugResponseHeaders will add to the debug output the HTTP response headers.
	DebugResponseHeaders
	// DebugDetailedTiming will add to the debug output the duration of the HTTP requests events.
	DebugDetailedTiming
)

// NewDebuggingRoundTripper allows to display in the logs output debug information
// on the API requests performed by the client.
func NewDebuggingRoundTripper(rt http.RoundTripper, logger zerolog.Logger, levels ...DebugLevel) http.RoundTripper {
	drt := &debuggingRoundTripper{
		delegatedRoundTripper: rt,
		logger:                logger,
		levels:                make(map[DebugLevel]bool, len(levels)),
	}
	for _, v := range levels {
		drt.levels[v] = true
	}
	return drt
}

// requestInfo keeps track of information about a request/response combination
type requestInfo struct {
	RequestHeaders http.Header `datapolicy:"token"`
	RequestVerb    string
	RequestURL     string

	ResponseStatus  string
	ResponseHeaders http.Header
	ResponseErr     error

	muTrace          sync.Mutex // Protect trace fields
	DNSLookup        time.Duration
	Dialing          time.Duration
	GetConnection    time.Duration
	TLSHandshake     time.Duration
	ServerProcessing time.Duration
	ConnectionReused bool

	Duration time.Duration
}

// newRequestInfo creates a new RequestInfo based on an http request
func newRequestInfo(req *http.Request) *requestInfo {
	return &requestInfo{
		RequestURL:     req.URL.String(),
		RequestVerb:    req.Method,
		RequestHeaders: req.Header,
	}
}

// complete adds information about the response to the requestInfo
func (r *requestInfo) complete(response *http.Response, err error) {
	if err != nil {
		r.ResponseErr = err
		return
	}
	r.ResponseStatus = response.Status
	r.ResponseHeaders = response.Header
}

// toCurl returns a string that can be run as a command in a terminal (minus the body)
func (r *requestInfo) toCurl() string {
	headers := ""
	for key, values := range r.RequestHeaders {
		for _, value := range values {
			value = maskValue(key, value)
			headers += fmt.Sprintf(` -H %q`, fmt.Sprintf("%s: %s", key, value))
		}
	}

	return fmt.Sprintf("curl -v -X%s %s '%s'", r.RequestVerb, headers, r.RequestURL)
}

var knownAuthTypes = map[string]bool{
	"bearer":    true,
	"basic":     true,
	"negotiate": true,
}

// maskValue masks credential content from authorization headers
// See https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Authorization
func maskValue(key string, value string) string {
	if !strings.EqualFold(key, "Authorization") {
		return value
	}
	if len(value) == 0 {
		return ""
	}
	var authType string
	if i := strings.Index(value, " "); i > 0 {
		authType = value[0:i]
	} else {
		authType = value
	}
	if !knownAuthTypes[strings.ToLower(authType)] {
		return "<masked>"
	}
	if len(value) > len(authType)+1 {
		value = authType + " <masked>"
	} else {
		value = authType
	}
	return value
}

func (rt *debuggingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	reqInfo := newRequestInfo(req)

	if rt.levels[DebugJustURL] {
		rt.logger.Info().Msgf("%s %s", reqInfo.RequestVerb, reqInfo.RequestURL)
	}
	if rt.levels[DebugCurlCommand] {
		rt.logger.Info().Msgf("%s", reqInfo.toCurl())
	}
	if rt.levels[DebugRequestHeaders] {
		rt.logger.Info().Msg("Request Headers:")
		for key, values := range reqInfo.RequestHeaders {
			for _, value := range values {
				value = maskValue(key, value)
				rt.logger.Info().Msgf("    %s: %s", key, value)
			}
		}
	}

	startTime := time.Now()

	if rt.levels[DebugDetailedTiming] {
		var getConn, dnsStart, dialStart, tlsStart, serverStart time.Time
		var host string
		trace := &httptrace.ClientTrace{
			// DNS
			DNSStart: func(info httptrace.DNSStartInfo) {
				reqInfo.muTrace.Lock()
				defer reqInfo.muTrace.Unlock()
				dnsStart = time.Now()
				host = info.Host
			},
			DNSDone: func(info httptrace.DNSDoneInfo) {
				reqInfo.muTrace.Lock()
				defer reqInfo.muTrace.Unlock()
				reqInfo.DNSLookup = time.Since(dnsStart)
				rt.logger.Info().Msgf("HTTP Trace: DNS Lookup for %s resolved to %v", host, info.Addrs)
			},
			// Dial
			ConnectStart: func(network, addr string) {
				reqInfo.muTrace.Lock()
				defer reqInfo.muTrace.Unlock()
				dialStart = time.Now()
			},
			ConnectDone: func(network, addr string, err error) {
				reqInfo.muTrace.Lock()
				defer reqInfo.muTrace.Unlock()
				reqInfo.Dialing = time.Since(dialStart)
				if err != nil {
					rt.logger.Info().Msgf("HTTP Trace: Dial to %s:%s failed: %v", network, addr, err)
				} else {
					rt.logger.Info().Msgf("HTTP Trace: Dial to %s:%s succeed", network, addr)
				}
			},
			// TLS
			TLSHandshakeStart: func() {
				tlsStart = time.Now()
			},
			TLSHandshakeDone: func(_ tls.ConnectionState, _ error) {
				reqInfo.muTrace.Lock()
				defer reqInfo.muTrace.Unlock()
				reqInfo.TLSHandshake = time.Since(tlsStart)
			},
			// Connection (it can be DNS + Dial or just the time to get one from the connection pool)
			GetConn: func(hostPort string) {
				getConn = time.Now()
			},
			GotConn: func(info httptrace.GotConnInfo) {
				reqInfo.muTrace.Lock()
				defer reqInfo.muTrace.Unlock()
				reqInfo.GetConnection = time.Since(getConn)
				reqInfo.ConnectionReused = info.Reused
			},
			// Server Processing (time since we wrote the request until first byte is received)
			WroteRequest: func(info httptrace.WroteRequestInfo) {
				reqInfo.muTrace.Lock()
				defer reqInfo.muTrace.Unlock()
				serverStart = time.Now()
			},
			GotFirstResponseByte: func() {
				reqInfo.muTrace.Lock()
				defer reqInfo.muTrace.Unlock()
				reqInfo.ServerProcessing = time.Since(serverStart)
			},
		}
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	}

	response, err := rt.delegatedRoundTripper.RoundTrip(req)
	reqInfo.Duration = time.Since(startTime)

	reqInfo.complete(response, err)

	if rt.levels[DebugURLTiming] {
		rt.logger.Info().Msgf("%s %s %s in %d milliseconds", reqInfo.RequestVerb, reqInfo.RequestURL, reqInfo.ResponseStatus, reqInfo.Duration.Nanoseconds()/int64(time.Millisecond))
	}
	if rt.levels[DebugDetailedTiming] {
		stats := ""
		if !reqInfo.ConnectionReused {
			stats += fmt.Sprintf(`DNSLookup %d ms Dial %d ms TLSHandshake %d ms`,
				reqInfo.DNSLookup.Nanoseconds()/int64(time.Millisecond),
				reqInfo.Dialing.Nanoseconds()/int64(time.Millisecond),
				reqInfo.TLSHandshake.Nanoseconds()/int64(time.Millisecond),
			)
		} else {
			stats += fmt.Sprintf(`GetConnection %d ms`, reqInfo.GetConnection.Nanoseconds()/int64(time.Millisecond))
		}
		if reqInfo.ServerProcessing != 0 {
			stats += fmt.Sprintf(` ServerProcessing %d ms`, reqInfo.ServerProcessing.Nanoseconds()/int64(time.Millisecond))
		}
		stats += fmt.Sprintf(` Duration %d ms`, reqInfo.Duration.Nanoseconds()/int64(time.Millisecond))
		rt.logger.Info().Msgf("HTTP Statistics: %s", stats)
	}

	if rt.levels[DebugResponseStatus] {
		rt.logger.Info().Msgf("Response Status: %s in %d milliseconds", reqInfo.ResponseStatus, reqInfo.Duration.Nanoseconds()/int64(time.Millisecond))
	}
	if rt.levels[DebugResponseHeaders] {
		rt.logger.Info().Msg("Response Headers:")
		for key, values := range reqInfo.ResponseHeaders {
			for _, value := range values {
				rt.logger.Info().Msgf("    %s: %s", key, value)
			}
		}
	}

	return response, err
}
