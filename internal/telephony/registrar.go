package telephony

import (
	"fmt"
	"net"
)

// ResolveRegistrar looks up the registrar and reports the addresses it resolves to.
//
// This exists because ".box" is a real public gTLD: on a network whose DNS does not
// answer for "fritz.box" locally, the name resolves to a stranger's host on the
// internet and REGISTER packets — carrying the SIP username — are sent there. The
// symptom is a bare transaction timeout that looks like a SIP fault, so resolution is
// checked and logged up front instead.
func ResolveRegistrar(host string, allowPublic bool) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, checkPrivate(host, []net.IP{ip}, allowPublic)
	}

	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, fmt.Errorf("resolving registrar %q: %w", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("registrar %q resolved to no addresses", host)
	}
	return ips, checkPrivate(host, ips, allowPublic)
}

func checkPrivate(host string, ips []net.IP, allowPublic bool) error {
	if allowPublic {
		return nil
	}
	for _, ip := range ips {
		if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			return nil
		}
	}
	return fmt.Errorf(
		"registrar %q resolves to %s, which is not a private address: refusing to send "+
			"SIP credentials to a host outside your LAN. A Fritz!Box is normally reachable "+
			"at a 192.168.x.x address — set PHONELLM_SIP_REGISTRAR to that IP. "+
			"(\".box\" is a public gTLD, so \"fritz.box\" resolves on the internet when your "+
			"local DNS does not answer for it.) Set PHONELLM_ALLOW_PUBLIC_REGISTRAR=true if "+
			"you really do mean an external SIP provider",
		host, joinIPs(ips))
}

func joinIPs(ips []net.IP) string {
	out := ""
	for i, ip := range ips {
		if i > 0 {
			out += ", "
		}
		out += ip.String()
	}
	return out
}
