package client

import (
	"net/http"
	"testing"
	"time"

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
		"valid port with error stream": {
			streamType: "error",
			requestID:  "42",
		},
		"valid port with data stream": {
			streamType: "data",
			requestID:  "42",
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

func TestGetStreamPair(t *testing.T) {
	timeout := make(chan time.Time)

	conn := &fakeConn{}
	h := &httpStreamHandler{
		streamPairs: make(map[string]*httpStreamPair),
		conn:        conn,
	}

	// test adding a new entry
	p, created := h.getStreamPair("1")
	if p == nil {
		t.Fatalf("unexpected nil pair")
	}
	if !created {
		t.Fatal("expected created=true")
	}
	if p.dataStream != nil {
		t.Errorf("unexpected non-nil data stream")
	}
	if p.errorStream != nil {
		t.Errorf("unexpected non-nil error stream")
	}

	// start the monitor for this pair
	monitorDone := make(chan struct{})
	go func() {
		h.monitorStreamPair(p, timeout)
		close(monitorDone)
	}()

	if !h.hasStreamPair("1") {
		t.Fatal("This should still be true")
	}

	// make sure we can retrieve an existing entry
	p2, created := h.getStreamPair("1")
	if created {
		t.Fatal("expected created=false")
	}
	if p != p2 {
		t.Fatalf("retrieving an existing pair: expected %#v, got %#v", p, p2)
	}

	// removed via complete
	dataStream := newFakeHTTPStream()
	dataStream.headers.Set(api.HeaderStreamType, api.StreamTypeData)
	complete, err := p.add(dataStream)
	if err != nil {
		t.Fatalf("unexpected error adding data stream to pair: %v", err)
	}
	if complete {
		t.Fatalf("unexpected complete")
	}

	errorStream := newFakeHTTPStream()
	errorStream.headers.Set(api.HeaderStreamType, api.StreamTypeError)
	complete, err = p.add(errorStream)
	if err != nil {
		t.Fatalf("unexpected error adding error stream to pair: %v", err)
	}
	if !complete {
		t.Fatal("unexpected incomplete")
	}

	// make sure monitorStreamPair completed
	<-monitorDone

	if !conn.removeStreamsCalled {
		t.Fatalf("connection remove stream not called")
	}
	conn.removeStreamsCalled = false

	// make sure the pair was removed
	if h.hasStreamPair("1") {
		t.Fatal("expected removal of pair after both data and error streams received")
	}

	// removed via timeout
	p, created = h.getStreamPair("2")
	if !created {
		t.Fatal("expected created=true")
	}
	if p == nil {
		t.Fatal("expected p not to be nil")
	}

	monitorDone = make(chan struct{})
	go func() {
		h.monitorStreamPair(p, timeout)
		close(monitorDone)
	}()
	// cause the timeout
	close(timeout)
	// make sure monitorStreamPair completed
	<-monitorDone
	if h.hasStreamPair("2") {
		t.Fatal("expected stream pair to be removed")
	}
	if !conn.removeStreamsCalled {
		t.Fatalf("connection remove stream not called")
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
