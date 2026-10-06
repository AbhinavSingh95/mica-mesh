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
	choices, err := candidates(ctx, browse)
	if err != nil {
		return "", err
	}
	switch len(choices) {
	case 0:
		return "", errors.New("no compatible controller found; use --controller-address HOST:PORT")
	case 1:
		return choices[0].Address, nil
	default:
		addresses := make([]string, len(choices))
		for i, choice := range choices {
			addresses[i] = choice.Address
		}
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

// Candidate is one compatible Controller identity and endpoint.
type Candidate struct{ InstanceID, Hostname, Address string }

// Candidates gathers a bounded, sorted snapshot of compatible LAN Controllers.
func Candidates(ctx context.Context) ([]Candidate, error) { return candidates(ctx, browseMDNS) }
func candidates(ctx context.Context, browse browseFunc) ([]Candidate, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	window, cancel := context.WithTimeout(ctx, browseBudget)
	defer cancel()
	candidates := make(map[uuid.UUID]Candidate)
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
		choice := Candidate{InstanceID: id.String(), Hostname: info.Hostname, Address: address}
		if previous, ok := candidates[id]; !ok || address < previous.Address || address == previous.Address && choice.Hostname < previous.Hostname {
			candidates[id] = choice
		}
	})
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if overflow {
		return nil, errors.New("too many discovery advertisements; select --controller-address HOST:PORT")
	}
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return nil, fmt.Errorf("browse controllers: %w; use --controller-address HOST:PORT", err)
	}

	result := make([]Candidate, 0, len(candidates))
	for _, choice := range candidates {
		result = append(result, choice)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Address != result[j].Address {
			return result[i].Address < result[j].Address
		}
		return result[i].InstanceID < result[j].InstanceID
	})
	return result, nil
}

// InterfaceAddress is one usable local LAN IPv4 and its interface name.
type InterfaceAddress struct{ Name, IPv4 string }

// InterfaceAddresses returns named LAN IPv4 choices without requiring multicast.
func InterfaceAddresses() ([]InterfaceAddress, error) {
	addresses, err := localAddresses()
	if err != nil {
		return nil, err
	}
	return interfaceAddresses(addresses), nil
}
func interfaceAddresses(addresses []localAddress) []InterfaceAddress {
	var result []InterfaceAddress
	for _, address := range addresses {
		if address.iface.Flags&net.FlagUp != 0 && remoteIPv4(address.ip) {
			result = append(result, InterfaceAddress{Name: address.iface.Name, IPv4: address.ip.String()})
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Name != result[j].Name {
			return result[i].Name < result[j].Name
		}
		return result[i].IPv4 < result[j].IPv4
	})
	return result
}
