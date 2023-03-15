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

type Config struct {
	DialEndpoint *url.URL // e.g. https://<public-ip>:12345/tunnel
	DialMethod   string   // e.g. POST
	LocalPort    int
}

type PortForwarderClient interface {
	Open(ctx context.Context)
	ForwardData(stream httpstream.Stream) error
	Close()
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

func New(log zerolog.Logger, cfg *Config) (PortForwarderClient, error) {
	tun := &portForwardClientImpl{
		log:       log,
		stopChan:  make(chan struct{}, 1),
		localPort: cfg.LocalPort,
		streams:   make(chan httpstream.Stream, 1),
	}
	transport, upgrader, err := rt.RoundTripperFor(log, getNewStreamHandler(tun.streams))
	if err != nil {
		return nil, err
	}
	tun.dialer = rt.NewDialer(upgrader, &http.Client{Transport: transport}, cfg.DialMethod, cfg.DialEndpoint)

	return tun, nil
}

func (t *portForwardClientImpl) Open(ctx context.Context) {
	go func() {
		<-ctx.Done()
		t.Close()
	}()

	// TODO: add restore connection logic if needed

	err := t.connectToRemote()
	if err != nil {
		t.log.Debug().Err(err).Msg("Port forward finished or errored")
	}
}

func (t *portForwardClientImpl) Close() {
	// TODO:
	t.stopChan <- struct{}{}
	if t.streamConn != nil {
		t.streamConn.Close()
	}
}

// connectToRemote formats and executes a port forwarding request. The connection will remain
// open until stopChan is closed.
func (t *portForwardClientImpl) connectToRemote() error {
	var err error
	t.streamConn, _, err = t.dialer.Dial(api.ProtocolName)
	if err != nil {
		return fmt.Errorf("error upgrading connection: %s", err)
	}
	defer func() {
		t.streamConn.Close()
		t.streamConn = nil
	}()

	const streamCreationTimeout = 30 * time.Second

	h := &httpStreamHandler{
		log:                   t.log,
		conn:                  t.streamConn,
		streamChan:            t.streams,
		streamPairs:           make(map[string]*httpStreamPair),
		streamCreationTimeout: streamCreationTimeout,
		dataForwarder:         t,
	}
	h.run() // todo: pass ctx?
	return nil
}

func (t *portForwardClientImpl) ForwardData(stream httpstream.Stream) error {
	ctx := context.TODO() // TODO:
	defer stream.Close()
	// TODO: hardcoded to tcp4 because localhost resolves to ::1 by default if the system has IPv6 enabled.
	// Theoretically happy eyeballs will try IPv6 first and fallback to IPv4
	// but resolving localhost doesn't seem to return and IPv4 address, thus failing the connection.
	conn, err := net.Dial("tcp4", fmt.Sprintf("127.0.0.1:%d", t.localPort))
	if err != nil {
		err = fmt.Errorf("failed to dial %d: %s", t.localPort, err.Error())
		return err
	}
	defer conn.Close()

	errCh := make(chan error, 2)
	// Copy from the local port connection to the client stream
	go func() {
		t.log.Debug().Msgf("PortForward copying data to the client stream")
		_, err := io.Copy(stream, conn)
		errCh <- err
	}()

	// Copy from the client stream to the port connection
	go func() {
		t.log.Debug().Msg("PortForward copying data from client stream to local port")
		_, err := io.Copy(conn, stream)
		errCh <- err
	}()

	// Wait until the first error is returned by one of the connections
	// we use errFwd to store the result of the port forwarding operation
	// if the context is cancelled close everything and return
	var errFwd error
	select {
	case errFwd = <-errCh:
		t.log.Debug().Err(err).Msg("PortForward stopped (one direction)")
	case <-ctx.Done():
		t.log.Debug().Err(err).Msg("PortForward cancelled (one direction)")
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
		t.log.Debug().Err(err).Msg("PortForward stopped forwarding in both directions")
	case <-time.After(timeout):
		t.log.Debug().Msg("PortForward timed out waiting to close the connection")
	case <-ctx.Done():
		t.log.Debug().Err(err).Msg("PortForward cancelled")
		errFwd = ctx.Err()
	}

	return errFwd
}
