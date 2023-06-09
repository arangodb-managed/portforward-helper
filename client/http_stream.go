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
	"sync"

	"errors"

	"github.com/arangodb-managed/portforward-helper/api"
	"github.com/arangodb-managed/portforward-helper/httpstream"
)

// httpStream represents the error and data streams for a port
// forwarding request.
type httpStream struct {
	lock       sync.RWMutex
	requestID  string
	dataStream httpstream.Stream
	complete   chan struct{}
}

// newPortForwardStream creates a new httpStream.
func newPortForwardStream(requestID string) *httpStream {
	return &httpStream{
		requestID: requestID,
		complete:  make(chan struct{}),
	}
}

// add adds the stream to the httpStream. If it already
// contains a stream for the new stream's type, an error is returned. add
// returns true if all required streams are received.
func (p *httpStream) add(stream httpstream.Stream) (bool, error) {
	p.lock.Lock()
	defer p.lock.Unlock()

	switch stream.Headers().Get(api.HeaderStreamType) {
	case api.StreamTypeData:
		if p.dataStream != nil {
			return false, errors.New("data stream already assigned")
		}
		p.dataStream = stream
	}

	complete := p.dataStream != nil
	if complete {
		close(p.complete)
	}
	return complete, nil
}
