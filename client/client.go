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
	// Debug enables debug logging
	Debug bool
}

// PortForwarderClient allows the remote service to bypass the client firewall by using long-running connection
type PortForwarderClient interface {
	// StartForwarding starts a goroutine which initiates the connection to remote service.
	//It will try to re-connect in case of failure.
	StartForwarding(ctx context.Context, minRetryInterval, maxRetryInterval time.Duration)
	// Ready returns true if client currently has an active forwarding connection
	Ready() bool
	// Close terminates the connection
	Close() error
}

type portForwardClientImpl struct {
	log      zerolog.Logger
	stopChan chan struct{}
	streams  chan httpstream.Stream

	requestDecorator        func(req *http.Request)
	forwardAddr             string
	forwardPort             int
	dialer                  httpstream.Dialer
	streamConn              httpstream.Connection
	lastSuccessfulConnectAt time.Time
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
	var debugLogger *zerolog.Logger
	if cfg.Debug {
		debugLogger = &log
	}
	transport, upgrader, err := rt.RoundTripperFor(debugLogger, getNewStreamHandler(c.streams))
	if err != nil {
		return nil, err
	}
	c.dialer = rt.NewDialer(upgrader, &http.Client{Transport: transport}, cfg.DialMethod, dialEndpoint)

	return c, nil
}

func (c *portForwardClientImpl) StartForwarding(ctx context.Context, minRetryInterval, maxRetryInterval time.Duration) {
	retryIn := minRetryInterval
	attempt := 1
	for {
		err := c.connectToRemote(ctx)
		c.log.Debug().Err(err).Msg("Connection interrupted")

		if time.Since(c.lastSuccessfulConnectAt) < minRetryInterval {
			// reset retry interval
			retryIn = minRetryInterval
		} else if attempt != 1 {
			retryIn *= 2
			if retryIn > maxRetryInterval {
				retryIn = maxRetryInterval
			}
		}
		lastSuccessAt := "<never>"
		if !c.lastSuccessfulConnectAt.IsZero() {
			lastSuccessAt = c.lastSuccessfulConnectAt.String()
		}
		c.log.Debug().
			Int("attempt", attempt).
			Str("last-success-at", lastSuccessAt).
			Dur("retry-in", retryIn).
			Msg("Retrying connection")
		t := time.NewTimer(retryIn)
		select {
		case <-ctx.Done():
			if !t.Stop() {
				<-t.C
			}
			c.log.Debug().Err(err).Msg("Context finished")
			return
		case <-c.stopChan:
			c.log.Debug().Msg("Forwarding stopped")
			return
		case <-t.C:
			// retry
			attempt++
		}
	}
}

func (c *portForwardClientImpl) Ready() bool {
	return c.streamConn != nil
}

func (c *portForwardClientImpl) Close() error {
	c.stopChan <- struct{}{}
	if c.streamConn != nil {
		err := c.streamConn.Close()
		c.streamConn = nil
		return err
	}
	return nil
}

func (c *portForwardClientImpl) connectToRemote(ctx context.Context) error {
	var err error
	c.streamConn, _, err = c.dialer.Dial(c.requestDecorator, api.ProtocolName)
	if err != nil {
		return fmt.Errorf("error upgrading connection: %s", err)
	}
	defer func() {
		c.streamConn.Close()
		c.streamConn = nil
	}()
	c.lastSuccessfulConnectAt = time.Now()

	const streamCreationTimeout = 30 * time.Second
	h := &httpStreamHandler{
		log:                   c.log,
		conn:                  c.streamConn,
		streamChan:            c.streams,
		streams:               make(map[string]*httpStream),
		streamCreationTimeout: streamCreationTimeout,
		dataForwarder:         c,
	}
	h.run(ctx)
	return nil
}

func (c *portForwardClientImpl) CopyToStream(ctx context.Context, stream httpstream.Stream) error {
	defer stream.Close()
	addr := fmt.Sprintf("%s:%d", c.forwardAddr, c.forwardPort)
	conn, err := net.Dial("tcp4", addr)
	if err != nil {
		err = fmt.Errorf("failed to dial %d: %s", c.forwardPort, err.Error())
		return err
	}
	defer conn.Close()

	errCh := make(chan error, 2)
	// Copy from the forward port connection to the client stream
	go func() {
		c.log.Debug().
			Uint32("stream-id", stream.Identifier()).
			Str("address", addr).
			Msg("PortForward copying data from client to stream")
		_, err := io.Copy(stream, conn)
		errCh <- err
	}()

	// Copy from the client stream to the port connection
	go func() {
		c.log.Debug().
			Uint32("stream-id", stream.Identifier()).
			Str("address", addr).
			Msg("PortForward copying data from stream to client")
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

	t := time.NewTimer(timeout)
	select {
	case e := <-errCh:
		if errFwd == nil {
			errFwd = e
		}
		c.log.Debug().Err(err).Msg("PortForward stopped forwarding in both directions")
	case <-t.C:
		c.log.Debug().Msg("PortForward timed out waiting to close the connection")
	case <-ctx.Done():
		if !t.Stop() {
			<-t.C
		}
		c.log.Debug().Err(err).Msg("PortForward cancelled")
		errFwd = ctx.Err()
	}

	return errFwd
}
