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

package client

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/rs/zerolog"

	"github.com/arangodb-managed/portforward-helper/api"
	"github.com/arangodb-managed/portforward-helper/client/internal/rt"
	"github.com/arangodb-managed/portforward-helper/httpstream"
)

// Config specifies the configuration for PortForwarderClient
type Config struct {
	// DialEndpoint must contain an HTTP URL which will be used for connection upgrade
	// e.g. https://<public-ip>:12345/tunnel
	DialEndpoint string
	// DialMethod must contain an HTTP method to be used for request, e.g. POST
	DialMethod string
	// LocalPort is the port at localhost which will receive the data stream from remote service
	LocalPort int
}

// PortForwarderClient allows the remote service to bypass the client firewall by using long-running connection
type PortForwarderClient interface {
	// Open initiates the connection to remote service
	Open(ctx context.Context)
	// Close terminates the connection
	Close() error
}

// portForwardClientImpl knows how to listen for local connections and forward them to
// a remote host via an upgraded HTTP request.
type portForwardClientImpl struct {
	log      zerolog.Logger
	stopChan chan struct{}
	streams  chan httpstream.Stream

	localPort  int
	dialer     httpstream.Dialer
	streamConn httpstream.Connection
}

// New creates PortForwarderClient
func New(log zerolog.Logger, cfg *Config) (PortForwarderClient, error) {
	dialEndpoint, err := url.Parse(cfg.DialEndpoint)
	if err != nil {
		return nil, fmt.Errorf("failed to parse DialEndpoint URL: %s", err)
	}
	if cfg.DialMethod == "" {
		return nil, fmt.Errorf("empty DialMethod")
	}
	c := &portForwardClientImpl{
		log:       log,
		stopChan:  make(chan struct{}, 1),
		localPort: cfg.LocalPort,
		streams:   make(chan httpstream.Stream, 1),
	}
	transport, upgrader, err := rt.RoundTripperFor(log, getNewStreamHandler(c.streams))
	if err != nil {
		return nil, err
	}
	c.dialer = rt.NewDialer(upgrader, &http.Client{Transport: transport}, cfg.DialMethod, dialEndpoint)

	return c, nil
}

func (c *portForwardClientImpl) Open(ctx context.Context) {
	go func() {
		<-ctx.Done()
		c.Close()
	}()

	// TODO: add restore connection logic if needed

	err := c.connectToRemote()
	if err != nil {
		c.log.Debug().Err(err).Msg("Port forward finished or errored")
	}
}

func (c *portForwardClientImpl) Close() error {
	// TODO:
	c.stopChan <- struct{}{}
	if c.streamConn != nil {
		return c.streamConn.Close()
	}
	return nil
}

// connectToRemote formats and executes a port forwarding request. The connection will remain
// open until stopChan is closed.
func (c *portForwardClientImpl) connectToRemote() error {
	var err error
	c.streamConn, _, err = c.dialer.Dial(api.ProtocolName)
	if err != nil {
		return fmt.Errorf("error upgrading connection: %s", err)
	}
	defer func() {
		c.streamConn.Close()
		c.streamConn = nil
	}()

	const streamCreationTimeout = 30 * time.Second

	h := &httpStreamHandler{
		log:                   c.log,
		conn:                  c.streamConn,
		streamChan:            c.streams,
		streamPairs:           make(map[string]*httpStreamPair),
		streamCreationTimeout: streamCreationTimeout,
		dataForwarder:         c,
	}
	h.run() // todo: pass ctx?
	return nil
}

func (c *portForwardClientImpl) CopyToStream(stream httpstream.Stream) error {
	ctx := context.TODO() // TODO:
	defer stream.Close()
	// TODO: hardcoded to tcp4 because localhost resolves to ::1 by default if the system has IPv6 enabled.
	// Theoretically happy eyeballs will try IPv6 first and fallback to IPv4
	// but resolving localhost doesn't seem to return and IPv4 address, thus failing the connection.
	conn, err := net.Dial("tcp4", fmt.Sprintf("127.0.0.1:%d", c.localPort))
	if err != nil {
		err = fmt.Errorf("failed to dial %d: %s", c.localPort, err.Error())
		return err
	}
	defer conn.Close()

	errCh := make(chan error, 2)
	// Copy from the local port connection to the client stream
	go func() {
		c.log.Debug().Msgf("PortForward copying data to the client stream")
		_, err := io.Copy(stream, conn)
		errCh <- err
	}()

	// Copy from the client stream to the port connection
	go func() {
		c.log.Debug().Msg("PortForward copying data from client stream to local port")
		_, err := io.Copy(conn, stream)
		errCh <- err
	}()

	// Wait until the first error is returned by one of the connections
	// we use errFwd to store the result of the port forwarding operation
	// if the context is cancelled close everything and return
	var errFwd error
	select {
	case errFwd = <-errCh:
		c.log.Debug().Err(err).Msg("PortForward stopped (one direction)")
	case <-ctx.Done():
		c.log.Debug().Err(err).Msg("PortForward cancelled (one direction)")
		return ctx.Err()
	}
	// give a chance to terminate gracefully or timeout
	// 0.5s is the default timeout used in socat
	// https://linux.die.net/man/1/socat
	timeout := time.Duration(500) * time.Millisecond
	select {
	case e := <-errCh:
		if errFwd == nil {
			errFwd = e
		}
		c.log.Debug().Err(err).Msg("PortForward stopped forwarding in both directions")
	case <-time.After(timeout):
		c.log.Debug().Msg("PortForward timed out waiting to close the connection")
	case <-ctx.Done():
		c.log.Debug().Err(err).Msg("PortForward cancelled")
		errFwd = ctx.Err()
	}

	return errFwd
}
