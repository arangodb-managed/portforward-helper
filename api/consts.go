package api

import "log"

const (
	// HeaderRequestID is the name of header that specifies a request ID used to associate the error
	// and data streams for a single forwarded connection
	HeaderRequestID = "requestID"
	// HeaderStreamType Name of header that specifies stream type
	HeaderStreamType = "streamType"
)
const (
	StreamTypeData  = "data"
	StreamTypeError = "error"
)

// ProtocolName is the subprotocol used for port forwarding.
const ProtocolName = "PortForwarding-v1"

// TODO: rename

func HandleError(err error) {
	log.Printf("Got another err %s\n", err.Error())
}
