package telephony

import (
	"strings"
	"testing"
)

// Literal IPs keep these tests off the network.
func TestResolveRegistrarAcceptsPrivateAddresses(t *testing.T) {
	for _, host := range []string{"192.168.1.1", "10.0.0.1", "172.16.5.4", "127.0.0.1"} {
		ips, err := ResolveRegistrar(host, false)
		if err != nil {
			t.Errorf("%s rejected: %v", host, err)
		}
		if len(ips) != 1 || ips[0].String() != host {
			t.Errorf("%s resolved to %v", host, ips)
		}
	}
}

// The failure this guard exists for: ".box" is a public gTLD, so on a network whose DNS
// does not answer locally, "fritz.box" resolves to a stranger and REGISTER leaks the
// SIP username to them.
func TestResolveRegistrarRejectsPublicAddress(t *testing.T) {
	_, err := ResolveRegistrar("212.42.244.122", false)
	if err == nil {
		t.Fatal("public registrar accepted; SIP credentials would leave the LAN")
	}
	if !strings.Contains(err.Error(), "PHONELLM_SIP_REGISTRAR") {
		t.Errorf("error should tell the user how to fix it, got: %v", err)
	}
}

func TestResolveRegistrarAllowsPublicWhenOptedIn(t *testing.T) {
	if _, err := ResolveRegistrar("212.42.244.122", true); err != nil {
		t.Fatalf("opt-in should permit an external SIP provider: %v", err)
	}
}

func TestResolveRegistrarReportsLookupFailure(t *testing.T) {
	if _, err := ResolveRegistrar("no-such-host.invalid", false); err == nil {
		t.Fatal("expected an error for an unresolvable registrar")
	}
}
