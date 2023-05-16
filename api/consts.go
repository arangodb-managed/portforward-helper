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

package api

const (
	// HeaderRequestID is the name of header that specifies a request ID used to associate the error
	// and data streams for a single forwarded connection
	HeaderRequestID = "requestID"
	// HeaderStreamType name of header that specifies stream type
	HeaderStreamType = "streamType"
)
const (
	StreamTypeData = "data"
)

// ProtocolName is the subprotocol used for port forwarding.
const ProtocolName = "PortForwarding-v1"
