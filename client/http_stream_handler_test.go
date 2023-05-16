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
	"net/http"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/arangodb-managed/portforward-helper/api"
	"github.com/arangodb-managed/portforward-helper/httpstream"
)

func TestHTTPStreamReceived(t *testing.T) {
	tests := map[string]struct {
		streamType    string
		expectedError string
		requestID     string
	}{
		"missing stream type": {
			expectedError: `"streamType" header is required`,
			requestID:     "42",
		},
		"missing request ID": {
			expectedError: `"requestID" header is required`,
			streamType:    "data",
		},
		"invalid stream type": {
			streamType:    "foo",
			requestID:     "42",
			expectedError: `invalid stream type "foo"`,
		},
	}
	for name, test := range tests {
		streams := make(chan httpstream.Stream, 1)
		f := getNewStreamHandler(streams)
		stream := newFakeHTTPStream()
		if len(test.streamType) > 0 {
			stream.headers.Set("streamType", test.streamType)
		}
		if len(test.requestID) > 0 {
			stream.headers.Set("requestID", test.streamType)
		}
		replySent := make(chan struct{})
		err := f(stream, replySent)
		close(replySent)
		if len(test.expectedError) > 0 {
			if err == nil {
				t.Errorf("%s: expected err=%q, but it was nil", name, test.expectedError)
			}
			if e, a := test.expectedError, err.Error(); e != a {
				t.Errorf("%s: expected err=%q, got %q", name, e, a)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error %v", name, err)
			continue
		}
		if s := <-streams; s != stream {
			t.Errorf("%s: expected stream %#v, got %#v", name, stream, s)
		}
	}
}

type fakeConn struct {
	removeStreamsCalled bool
}

func (*fakeConn) CreateStream(headers http.Header) (httpstream.Stream, error) { return nil, nil }
func (*fakeConn) Close() error                                                { return nil }
func (*fakeConn) CloseChan() <-chan bool                                      { return nil }
func (*fakeConn) SetIdleTimeout(timeout time.Duration)                        {}
func (f *fakeConn) RemoveStreams(streams ...httpstream.Stream)                { f.removeStreamsCalled = true }

func TestGetStream(t *testing.T) {
	timeout := make(chan time.Time)

	conn := &fakeConn{}
	h := &httpStreamHandler{
		log:     zerolog.New(zerolog.NewTestWriter(t)),
		streams: make(map[string]*httpStream),
		conn:    conn,
	}

	// test adding a new entry
	p, created := h.getStream("1")
	if p == nil {
		t.Fatal("unexpected nil")
	}
	if !created {
		t.Fatal("expected created=true")
	}
	if p.dataStream != nil {
		t.Error("unexpected non-nil data stream")
	}

	// start the monitor for this stream
	monitorDone := make(chan struct{})
	go func() {
		h.monitorStream(p, timeout)
		close(monitorDone)
	}()

	if !h.hasStream("1") {
		t.Fatal("This should still be true")
	}

	// make sure we can retrieve an existing entry
	p2, created := h.getStream("1")
	if created {
		t.Fatal("expected created=false")
	}
	if p != p2 {
		t.Fatalf("retrieving an existing stream: expected %#v, got %#v", p, p2)
	}

	// removed via complete
	dataStream := newFakeHTTPStream()
	dataStream.headers.Set(api.HeaderStreamType, api.StreamTypeData)
	complete, err := p.add(dataStream)
	if err != nil {
		t.Fatalf("unexpected error adding data stream: %v", err)
	}
	if !complete {
		t.Fatal("expected complete=true")
	}

	// make sure monitorStream completed
	<-monitorDone

	if !conn.removeStreamsCalled {
		t.Fatal("connection remove stream not called")
	}
	conn.removeStreamsCalled = false

	// make sure the stream was removed
	if h.hasStream("1") {
		t.Fatal("expected removal of stream after data stream received")
	}

	// removed via timeout
	p, created = h.getStream("2")
	if !created {
		t.Fatal("expected created=true")
	}
	if p == nil {
		t.Fatal("expected p not to be nil")
	}

	monitorDone = make(chan struct{})
	go func() {
		h.monitorStream(p, timeout)
		close(monitorDone)
	}()
	// cause the timeout
	close(timeout)
	// make sure monitorStream completed
	<-monitorDone
	if h.hasStream("2") {
		t.Fatal("expected stream to be removed")
	}
	if !conn.removeStreamsCalled {
		t.Fatal("connection remove stream not called")
	}
}

type fakeHTTPStream struct {
	headers http.Header
	id      uint32
}

func newFakeHTTPStream() *fakeHTTPStream {
	return &fakeHTTPStream{
		headers: make(http.Header),
	}
}

var _ httpstream.Stream = &fakeHTTPStream{}

func (s *fakeHTTPStream) Read(data []byte) (int, error) {
	return 0, nil
}

func (s *fakeHTTPStream) Write(data []byte) (int, error) {
	return 0, nil
}

func (s *fakeHTTPStream) Close() error {
	return nil
}

func (s *fakeHTTPStream) Reset() error {
	return nil
}

func (s *fakeHTTPStream) Headers() http.Header {
	return s.headers
}

func (s *fakeHTTPStream) Identifier() uint32 {
	return s.id
}
