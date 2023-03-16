//
// DISCLAIMER
//
// Copyright 2023 ArangoDB GmbH, Cologne, Germany
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// Copyright holder is ArangoDB GmbH, Cologne, Germany
//

package rt

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/rs/zerolog"

	"github.com/arangodb-managed/portforward-helper/httpstream"
	"github.com/arangodb-managed/portforward-helper/httpstream/spdy"
)

// Upgrader validates a response from the server after a SPDY upgrade.
type Upgrader interface {
	// NewConnection validates the response and creates a new Connection.
	NewConnection(resp *http.Response) (httpstream.Connection, error)
}

// RoundTripperFor returns a round tripper and Upgrader to use with SPDY.
func RoundTripperFor(logger zerolog.Logger, newStreamHandler httpstream.NewStreamHandler) (http.RoundTripper, Upgrader, error) {
	tlsConfig := &tls.Config{
		// Can't use SSLv3 because of POODLE and BEAST
		// Can't use TLSv1.0 because of POODLE and BEAST using CBC cipher
		// Can't use TLSv1.1 because of RC4 cipher usage
		MinVersion: tls.VersionTLS12,
	}
	upgradeRoundTripper := spdy.NewRoundTripperWithConfig(spdy.RoundTripperConfig{
		TLS:              tlsConfig,
		PingPeriod:       time.Second * 5,
		NewStreamHandler: newStreamHandler,
	})
	wrapper := NewDebuggingRoundTripper(upgradeRoundTripper, logger, DebugJustURL, DebugRequestHeaders, DebugResponseStatus, DebugResponseHeaders)

	return wrapper, upgradeRoundTripper, nil
}

// dialer implements the httpstream.Dialer interface.
type dialer struct {
	client   *http.Client
	upgrader Upgrader
	method   string
	url      *url.URL
}

var _ httpstream.Dialer = &dialer{}

// NewDialer will create a dialer that connects to the provided URL and upgrades the connection to SPDY.
func NewDialer(upgrader Upgrader, client *http.Client, method string, url *url.URL) httpstream.Dialer {
	return &dialer{
		client:   client,
		upgrader: upgrader,
		method:   method,
		url:      url,
	}
}

func (d *dialer) Dial(protocols ...string) (httpstream.Connection, string, error) {
	req, err := http.NewRequest(d.method, d.url.String(), nil)
	if err != nil {
		return nil, "", fmt.Errorf("error creating request: %v", err)
	}
	return negotiate(d.upgrader, d.client, req, protocols...)
}

// negotiate opens a connection to a remote server and attempts to negotiate
// a SPDY connection. Upon success, it returns the connection and the protocol selected by
// the server. The client transport must use the upgradeRoundTripper - see RoundTripperFor.
func negotiate(upgrader Upgrader, client *http.Client, req *http.Request, protocols ...string) (httpstream.Connection, string, error) {
	for i := range protocols {
		req.Header.Add(httpstream.HeaderProtocolVersion, protocols[i])
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("error sending request: %v", err)
	}
	defer resp.Body.Close()

	conn, err := upgrader.NewConnection(resp)
	if err != nil {
		return nil, "", err
	}
	return conn, resp.Header.Get(httpstream.HeaderProtocolVersion), nil
}
