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
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/arangodb-managed/portforward-helper/api"
	"github.com/arangodb-managed/portforward-helper/httpstream"
)

func getNewStreamHandler(streams chan httpstream.Stream) func(httpstream.Stream, <-chan struct{}) error {
	return func(stream httpstream.Stream, _ <-chan struct{}) error {
		// make sure it has a valid stream type header
		streamType := stream.Headers().Get(api.HeaderStreamType)
		if streamType == "" {
			return fmt.Errorf("%q header is required", api.HeaderStreamType)
		}
		if stream.Headers().Get(api.HeaderRequestID) == "" {
			return fmt.Errorf("%q header is required", api.HeaderRequestID)
		}
		if streamType != api.StreamTypeData {
			return fmt.Errorf("invalid stream type %q", streamType)
		}

		streams <- stream
		return nil
	}
}

type dataForwarder interface {
	CopyToStream(ctx context.Context, stream httpstream.Stream) error
}

// httpStreamHandler is capable of processing multiple port forward
// requests over a single httpstream.Connection.
type httpStreamHandler struct {
	log                   zerolog.Logger
	conn                  httpstream.Connection
	streamChan            chan httpstream.Stream
	streamsLock           sync.RWMutex
	streams               map[string]*httpStream
	streamCreationTimeout time.Duration
	dataForwarder         dataForwarder
}

func (h *httpStreamHandler) handleStreamingError(err error) {
	h.log.Info().Err(err).Msg("streaming error")
}

// getStream returns a httpStream for requestID. The returned bool is
// true if the httpStream was created.
func (h *httpStreamHandler) getStream(requestID string) (*httpStream, bool) {
	h.streamsLock.Lock()
	defer h.streamsLock.Unlock()

	if p, ok := h.streams[requestID]; ok {
		h.log.Debug().Str("request_id", requestID).Msg("Connection request found existing stream")
		return p, false
	}

	h.log.Debug().Str("request_id", requestID).Msg("Connection request request creating new stream")

	p := newPortForwardStream(requestID)
	h.streams[requestID] = p

	return p, true
}

// monitorStream waits for the required streams or for the timeout to expire (whichever happens first), and then
// removes it.
func (h *httpStreamHandler) monitorStream(p *httpStream, timeout <-chan time.Time) {
	select {
	case <-timeout:
		err := fmt.Errorf("(conn=%+v, request=%s) timed out waiting for streams", h.conn, p.requestID)
		h.handleStreamingError(err)
	case <-p.complete:
		h.log.Debug().Str("request_id", p.requestID).Msg("Connection request successfully received all required streams")
	}
	h.removeStream(p.requestID)
}

// hasStream returns a bool indicating if a stream for requestID
// exists.
func (h *httpStreamHandler) hasStream(requestID string) bool {
	h.streamsLock.RLock()
	defer h.streamsLock.RUnlock()

	_, ok := h.streams[requestID]
	return ok
}

// removeStream removes the stream identified by requestID from streams.
func (h *httpStreamHandler) removeStream(requestID string) {
	h.streamsLock.Lock()
	defer h.streamsLock.Unlock()

	if h.conn != nil {
		s := h.streams[requestID]
		h.conn.RemoveStreams(s.dataStream)
	}
	delete(h.streams, requestID)
}

// run is the main loop for the httpStreamHandler. It processes new
// streams, invoking portForward for each complete stream. The loop exits
// when the httpstream.Connection is closed.
func (h *httpStreamHandler) run(ctx context.Context) {
	h.log.Debug().Msg("Connection waiting for port forward streams")
Loop:
	for {
		select {
		case <-h.conn.CloseChan():
			h.log.Debug().Msg("Upgraded connection closed")
			break Loop
		case stream := <-h.streamChan:
			requestID := stream.Headers().Get(api.HeaderRequestID)
			streamType := stream.Headers().Get(api.HeaderStreamType)
			h.log.Debug().
				Str("request_id", requestID).
				Str("stream_type", streamType).
				Msg("Connection request received")

			p, created := h.getStream(requestID)
			if created {
				go h.monitorStream(p, time.After(h.streamCreationTimeout))
			}
			if complete, err := p.add(stream); err != nil {
				err = fmt.Errorf("error processing stream for request %s: %v", requestID, err)
				h.handleStreamingError(err)
			} else if complete {
				go h.portForward(ctx, p)
			}
		}
	}
}

// portForward invokes the httpStreamHandler's forwarder.PortForward
// function for the given stream.
func (h *httpStreamHandler) portForward(ctx context.Context, p *httpStream) {
	err := h.dataForwarder.CopyToStream(ctx, p.dataStream)
	if err != nil {
		h.handleStreamingError(err)
	}
}
