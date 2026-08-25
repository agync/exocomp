package notify

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"syscall"
)

// metadataIP is the cloud instance-metadata address (AWS/GCP/Azure/OpenStack).
// It is already link-local, but it is named so a refusal can say why.
var metadataIP = net.IPv4(169, 254, 169, 254)

// deniedIP reports whether ip is one the daemon must never open a connection to
// on behalf of a client-supplied push endpoint, and a short reason for a log or
// error. The list is the SSRF deny-list: loopback, the cloud metadata address,
// link-local (169.254/16, fe80::/10), any multicast, RFC1918 + ULA (fc00::/7)
// private space, the unspecified address, and the IPv4 broadcast.
func deniedIP(ip net.IP) (string, bool) {
	if ip == nil {
		return "nil address", true
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	switch {
	case ip.IsUnspecified():
		return "unspecified address", true
	case ip.IsLoopback():
		return "loopback address", true
	case ip.Equal(metadataIP):
		return "cloud metadata address", true
	case ip.IsLinkLocalUnicast():
		return "link-local address", true
	case ip.IsLinkLocalMulticast(), ip.IsInterfaceLocalMulticast(), ip.IsMulticast():
		return "multicast address", true
	case ip.IsPrivate():
		return "private address", true
	case ip.Equal(net.IPv4bcast):
		return "broadcast address", true
	}
	return "", false
}

// lookupIP resolves a host to its IP addresses. It is a package var so a test
// can force a name to a chosen address without real DNS.
var lookupIP = func(ctx context.Context, host string) ([]net.IP, error) {
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	out := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.IP)
	}
	return out, nil
}

// ValidatePushEndpoint is the registration-time SSRF gate. It parses raw and
// refuses anything that is not a credential-free https URL whose host resolves
// exclusively to public addresses. It is the FIRST of two layers: it catches the
// obvious cases (literal private IPs, http, userinfo) at write time, but it is
// not sufficient on its own — DNS can change between here and the send, so the
// transport re-checks the actual connected IP at dial time (dialControl).
func ValidatePushEndpoint(ctx context.Context, raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("endpoint is not a URL: %w", err)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("endpoint scheme %q is not https", u.Scheme)
	}
	if u.User != nil {
		return nil, fmt.Errorf("endpoint must not carry credentials in the URL")
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("endpoint has no host")
	}

	// A literal IP is judged directly — it never goes through DNS, so the
	// dial-time guard is its only other check; refuse a private one here.
	if ip := net.ParseIP(host); ip != nil {
		if reason, denied := deniedIP(ip); denied {
			return nil, fmt.Errorf("endpoint host %s is a %s", host, reason)
		}
		return u, nil
	}

	ips, err := lookupIP(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("endpoint host %q does not resolve", host)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("endpoint host %q resolves to no address", host)
	}
	for _, ip := range ips {
		if reason, denied := deniedIP(ip); denied {
			return nil, fmt.Errorf("endpoint host %q resolves to a %s (%s)", host, reason, ip)
		}
	}
	return u, nil
}

// dialControl is the net.Dialer.Control hook the transport's HTTP client dials
// through. It runs AFTER DNS resolution, on the concrete address the socket is
// about to connect to, and refuses the connection if that IP is on the deny-list.
// This is the load-bearing SSRF defense: registration validates a name, but a
// name can be re-pointed at a private IP (DNS rebinding) after it is stored — the
// only check that cannot be fooled that way is the one on the address actually
// being dialed.
func dialControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("notify: push: refusing malformed dial address %q", address)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("notify: push: dial address %q is not a resolved IP", host)
	}
	if reason, denied := deniedIP(ip); denied {
		return fmt.Errorf("notify: push: refusing to connect to %s (%s)", ip, reason)
	}
	return nil
}
