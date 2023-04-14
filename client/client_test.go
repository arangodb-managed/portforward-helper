package client

import (
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func TestCreateNewClient(t *testing.T) {
	c, err := New(zerolog.New(zerolog.NewTestWriter(t)), &Config{
		DialEndpoint:     "192.168.1.100",
		DialMethod:       "GET",
		ForwardPort:      12345,
		RequestDecorator: nil,
	})
	require.NoError(t, err)
	require.NotNil(t, c)
}
