// Package discovery locates the single compatible controller on the local LAN.
package discovery

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/AbhinavSingh95/mica-mesh/internal/protocol"
	"github.com/google/uuid"
)

// ServiceName is the DNS-SD wire service shared by all mesh roles.
const ServiceName = "_mica-mesh._tcp.local."
const browseBudget = 3 * time.Second
const maxEntries = 256

// ControllerInfo identifies one controller process and its actual listening port.
type ControllerInfo struct {
	InstanceID    string
	Hostname      string
	IPv4          string
	Port          int
	ProtocolMajor uint32
}

// browseFunc calls emit synchronously and returns only after its resources stop.
type browseFunc func(context.Context, func(ControllerInfo)) error

// Resolve preserves an explicit address, or gathers LAN candidates for three
// seconds. Earlier caller cancellation/deadlines are preserved.
func Resolve(ctx context.Context, explicitAddress string) (string, error) {
	return resolve(ctx, explicitAddress, browseMDNS)
}
func resolve(ctx context.Context, explicit string, browse browseFunc) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if explicit != "" {
		return explicit, nil
	}
	window, cancel := context.WithTimeout(ctx, browseBudget)
	defer cancel()
	candidates := make(map[uuid.UUID]string)
	count := 0
	overflow := false
	err := browse(window, func(info ControllerInfo) {
		count++
		if count > maxEntries {
			overflow = true
			cancel()
			return
		}
		id, err := uuid.Parse(info.InstanceID)
		if err != nil || id == uuid.Nil || info.ProtocolMajor != protocol.Major || !remoteIPv4(net.ParseIP(info.IPv4)) || info.Port < 1 || info.Port > 65535 || info.Hostname == "" || len(info.Hostname) > 253 {
			return
		}
		address := net.JoinHostPort(net.ParseIP(info.IPv4).String(), fmt.Sprint(info.Port))
		if previous, ok := candidates[id]; !ok || address < previous {
			candidates[id] = address
		}
	})
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if overflow {
		return "", errors.New("too many discovery advertisements; select --controller-address HOST:PORT")
	}
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return "", fmt.Errorf("browse controllers: %w; use --controller-address HOST:PORT", err)
	}
	addresses := make([]string, 0, len(candidates))
	for _, address := range candidates {
		addresses = append(addresses, address)
	}
	sort.Strings(addresses)
	switch len(addresses) {
	case 0:
		return "", errors.New("no compatible controller found; use --controller-address HOST:PORT")
	case 1:
		return addresses[0], nil
	default:
		return "", fmt.Errorf("multiple controllers found (%s); select --controller-address HOST:PORT", strings.Join(addresses, ", "))
	}
}
func remoteIPv4(ip net.IP) bool {
	return ip.To4() != nil && !ip.IsLoopback() && (ip.IsGlobalUnicast() || ip.IsLinkLocalUnicast())
}

type localAddress struct {
	ip    net.IP
	iface net.Interface
}

func localAddresses() ([]localAddress, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var result []localAddress
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addresses, err := iface.Addrs()
		if err != nil {
			return nil, err
		}
		for _, address := range addresses {
			ip, _, err := net.ParseCIDR(address.String())
			if err == nil && ip.To4() != nil {
				result = append(result, localAddress{ip: ip, iface: iface})
			}
		}
	}
	return result, nil
}
func selectAddress(override string, listen net.IP, addresses []localAddress) (localAddress, error) {
	var matches []localAddress
	for _, address := range addresses {
		if !remoteIPv4(address.ip) {
			continue
		}
		if listen != nil && !listen.IsUnspecified() && !listen.Equal(address.ip) {
			continue
		}
		if override != "" && !address.ip.Equal(net.ParseIP(override)) {
			continue
		}
		matches = append(matches, address)
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return localAddress{}, errors.New("ambiguous LAN interface; select --advertise-address IPv4")
	}
	return localAddress{}, errors.New("no concrete local LAN IPv4 matches the listener; set --advertise-address IPv4 and a matching listener")
}

// LocalIPv4 selects the controller's reachable address, requiring an override
// when multiple active addresses fit its listener.
func LocalIPv4(override string, listen net.IP) (string, error) {
	addresses, err := localAddresses()
	if err != nil {
		return "", err
	}
	selected, err := selectAddress(override, listen, addresses)
	if err != nil {
		return "", err
	}
	return selected.ip.String(), nil
}

// ValidateLocalIPv4 checks that registration is local and fits its listener.
// Loopback is permitted only for a worker in the controller's own process.
func ValidateLocalIPv4(address string, listen net.IP, allowLoopback bool) error {
	ip := net.ParseIP(address)
	if !remoteIPv4(ip) && !(allowLoopback && ip.To4() != nil && ip.IsLoopback()) {
		return errors.New("worker needs a concrete LAN IPv4; use --advertise-address IPv4")
	}
	if listen != nil && !listen.IsUnspecified() && !listen.Equal(ip) {
		return errors.New("worker advertise address does not match its listener")
	}
	addresses, err := localAddresses()
	if err != nil {
		return err
	}
	for _, a := range addresses {
		if a.ip.Equal(ip) {
			return nil
		}
	}
	return errors.New("--advertise-address must exist on a local interface")
}
