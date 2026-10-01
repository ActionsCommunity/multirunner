package winvm

import (
	"net"
	"strconv"
	"testing"
)

func TestFreePortUsesRequestedBindAddress(t *testing.T) {
	// Loopback allocation would succeed despite this invalid advertised host.
	if _, err := freePort("invalid host"); err == nil {
		t.Fatal("ignored invalid bind address")
	}
	for _, host := range []string{"127.0.0.1", "::1"} {
		t.Run(host, func(t *testing.T) {
			probe, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
			if err != nil {
				t.Skipf("address unavailable: %v", err)
			}
			occupied := probe.Addr().(*net.TCPAddr).Port
			defer probe.Close()
			port, err := freePort(host, occupied)
			if err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
			if err != nil {
				t.Fatalf("allocated port unavailable on %s: %v", host, err)
			}
			listener.Close()
		})
	}
}
