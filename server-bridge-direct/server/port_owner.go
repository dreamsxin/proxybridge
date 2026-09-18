package server

import (
	"errors"
	"net"
	"strings"
)

// Limit diagnostics so a large sync with many occupied ports does not start
// one process scan per listener at the same time.
var portOwnerLookupSem = make(chan struct{}, 4)

func isAddressInUse(err error) bool {
	if errors.Is(err, net.ErrClosed) {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "address already in use") ||
		strings.Contains(message, "only one usage of each socket address")
}

func lookupPortOwner(port int) string {
	portOwnerLookupSem <- struct{}{}
	defer func() { <-portOwnerLookupSem }()
	return describePortOwner(port)
}
