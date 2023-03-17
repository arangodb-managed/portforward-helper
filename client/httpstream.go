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
	"os"
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
		if streamType != api.StreamTypeData && streamType != api.StreamTypeError {
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
	streamPairsLock       sync.RWMutex
	streamPairs           map[string]*httpStreamPair
	streamCreationTimeout time.Duration
	dataForwarder         dataForwarder
}

func (h *httpStreamHandler) handleStreamingError(err error) {
	h.log.Info().Err(err).Msg("streaming error")
}

// getStreamPair returns a httpStreamPair for requestID. This creates a
// new pair if one does not yet exist for the requestID. The returned bool is
// true if the pair was created.
func (h *httpStreamHandler) getStreamPair(requestID string) (*httpStreamPair, bool) {
	h.streamPairsLock.Lock()
	defer h.streamPairsLock.Unlock()

	if p, ok := h.streamPairs[requestID]; ok {
		h.log.Debug().Str("request_id", requestID).Msg("Connection request found existing stream pair")
		return p, false
	}

	h.log.Debug().Str("request_id", requestID).Msg("Connection request request creating new stream pair")

	p := newPortForwardPair(requestID)
	h.streamPairs[requestID] = p

	return p, true
}

// monitorStreamPair waits for the pair to receive both its error and data
// streams, or for the timeout to expire (whichever happens first), and then
// removes the pair.
func (h *httpStreamHandler) monitorStreamPair(p *httpStreamPair, timeout <-chan time.Time) {
	select {
	case <-timeout:
		err := fmt.Errorf("(conn=%v, request=%s) timed out waiting for streams", h.conn, p.requestID)
		h.handleStreamingError(err)
		p.printError(err.Error())
	case <-p.complete:
		h.log.Debug().Str("request_id", p.requestID).Msg("Connection request successfully received error and data streams")
	}
	h.removeStreamPair(p.requestID)
}

// hasStreamPair returns a bool indicating if a stream pair for requestID
// exists.
func (h *httpStreamHandler) hasStreamPair(requestID string) bool {
	h.streamPairsLock.RLock()
	defer h.streamPairsLock.RUnlock()

	_, ok := h.streamPairs[requestID]
	return ok
}

// removeStreamPair removes the stream pair identified by requestID from streamPairs.
func (h *httpStreamHandler) removeStreamPair(requestID string) {
	h.streamPairsLock.Lock()
	defer h.streamPairsLock.Unlock()

	if h.conn != nil {
		pair := h.streamPairs[requestID]
		h.conn.RemoveStreams(pair.dataStream, pair.errorStream)
	}
	delete(h.streamPairs, requestID)
}

// run is the main loop for the httpStreamHandler. It processes new
// streams, invoking portForward for each complete stream pair. The loop exits
// when the httpstream.Connection is closed.
func (h *httpStreamHandler) run(ctx context.Context) {
	h.log.Debug().Msg("Connection waiting for port forward streams")
Loop:
	for {
		select {
		case <-h.conn.CloseChan():
			h.log.Debug().Msg("Connection upgraded connection closed")
			break Loop
		case stream := <-h.streamChan:
			requestID := stream.Headers().Get(api.HeaderRequestID)
			streamType := stream.Headers().Get(api.HeaderStreamType)
			h.log.Debug().
				Str("request_id", requestID).
				Str("stream_type", streamType).
				Msg("Connection request received")

			p, created := h.getStreamPair(requestID)
			if created {
				go h.monitorStreamPair(p, time.After(h.streamCreationTimeout))
			}
			if complete, err := p.add(stream); err != nil {
				err = fmt.Errorf("error processing stream for request %s: %v", requestID, err)
				h.handleStreamingError(err)
				p.printError(err.Error())
			} else if complete {
				go h.portForward(ctx, p)
			}
		}
	}
}

// portForward invokes the httpStreamHandler's forwarder.PortForward
// function for the given stream pair.
func (h *httpStreamHandler) portForward(ctx context.Context, p *httpStreamPair) {
	// handle err stream
	go func() {
		defer p.errorStream.Close()
		_, err := io.Copy(os.Stderr, p.errorStream)
		if err != nil {
			h.handleStreamingError(err)
		}
	}()

	err := h.dataForwarder.CopyToStream(ctx, p.dataStream)
	if err != nil {
		h.handleStreamingError(err)
	}
}
