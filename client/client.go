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
	// ForwardAddr is an address which will receive the data stream from remote service
	// 127.0.0.1 if not specified
	ForwardAddr string
	// ForwardPort is the number of port which will receive the data stream from remote service
	ForwardPort int
	// RequestDecorator allows to modify the request before it is sent to the Port Forwarder
	RequestDecorator func(req *http.Request)
}

// PortForwarderClient allows the remote service to bypass the client firewall by using long-running connection
type PortForwarderClient interface {
	// Open initiates the connection to remote service.	readyChan will be closed on successful connection upgrade.
	Open(ctx context.Context, readyChan chan struct{})
	// Close terminates the connection
	Close() error
}

type portForwardClientImpl struct {
	log      zerolog.Logger
	stopChan chan struct{}
	streams  chan httpstream.Stream

	requestDecorator func(req *http.Request)
	forwardAddr      string
	forwardPort      int
	dialer           httpstream.Dialer
	streamConn       httpstream.Connection
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
	if cfg.ForwardPort < 1 {
		return nil, fmt.Errorf("invalid ForwardPort")
	}
	if cfg.ForwardAddr == "" {
		cfg.ForwardAddr = "127.0.0.1"
	}
	c := &portForwardClientImpl{
		log:              log,
		stopChan:         make(chan struct{}, 1),
		forwardAddr:      cfg.ForwardAddr,
		forwardPort:      cfg.ForwardPort,
		requestDecorator: cfg.RequestDecorator,
		streams:          make(chan httpstream.Stream, 1),
	}
	transport, upgrader, err := rt.RoundTripperFor(log, getNewStreamHandler(c.streams))
	if err != nil {
		return nil, err
	}
	c.dialer = rt.NewDialer(upgrader, &http.Client{Transport: transport}, cfg.DialMethod, dialEndpoint)

	return c, nil
}

func (c *portForwardClientImpl) Open(ctx context.Context, readyChan chan struct{}) {
	err := c.connectToRemote(ctx, readyChan)
	if err != nil {
		c.log.Debug().Err(err).Msg("Port forward finished or errored")
	}
}

func (c *portForwardClientImpl) Close() error {
	c.stopChan <- struct{}{}
	if c.streamConn != nil {
		return c.streamConn.Close()
	}
	return nil
}

func (c *portForwardClientImpl) connectToRemote(ctx context.Context, readyChan chan struct{}) error {
	var err error
	c.streamConn, _, err = c.dialer.Dial(c.requestDecorator, api.ProtocolName)
	if err != nil {
		return fmt.Errorf("error upgrading connection: %s", err)
	}
	defer func() {
		c.streamConn.Close()
		c.streamConn = nil
	}()

	close(readyChan)

	const streamCreationTimeout = 30 * time.Second
	h := &httpStreamHandler{
		log:                   c.log,
		conn:                  c.streamConn,
		streamChan:            c.streams,
		streamPairs:           make(map[string]*httpStreamPair),
		streamCreationTimeout: streamCreationTimeout,
		dataForwarder:         c,
	}
	h.run(ctx)
	return nil
}

func (c *portForwardClientImpl) CopyToStream(ctx context.Context, stream httpstream.Stream) error {
	defer stream.Close()
	conn, err := net.Dial("tcp4", fmt.Sprintf("%s:%d", c.forwardAddr, c.forwardPort))
	if err != nil {
		err = fmt.Errorf("failed to dial %d: %s", c.forwardPort, err.Error())
		return err
	}
	defer conn.Close()

	errCh := make(chan error, 2)
	// Copy from the forward port connection to the client stream
	go func() {
		c.log.Debug().Msgf("PortForward copying data to the client stream")
		_, err := io.Copy(stream, conn)
		errCh <- err
	}()

	// Copy from the client stream to the port connection
	go func() {
		c.log.Debug().Msg("PortForward copying data from client stream to forward port")
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
