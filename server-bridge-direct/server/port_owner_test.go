package server

import (
	"errors"
	"net"
	"testing"
)

func TestIsAddressInUse(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "go bind error", err: errors.New("listen tcp :6538: bind: address already in use"), want: true},
		{name: "windows bind error", err: errors.New("listen tcp :6538: bind: Only one usage of each socket address (protocol/network address/port) is normally permitted."), want: true},
		{name: "closed listener", err: net.ErrClosed, want: false},
		{name: "other error", err: errors.New("permission denied"), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isAddressInUse(tt.err); got != tt.want {
				t.Fatalf("isAddressInUse(%q) = %t, want %t", tt.err, got, tt.want)
			}
		})
	}
}
