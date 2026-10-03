package discovery

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/AbhinavSingh95/mica-mesh/internal/protocol"
	"github.com/google/uuid"
	"github.com/libp2p/zeroconf/v2"
)

const service = "_mica-mesh._tcp"
const domain = "local."

func browseMDNS(ctx context.Context, emit func(ControllerInfo)) error {
	addresses, err := localAddresses()
	if err != nil {
		return err
	}
	interfaces, err := browseInterfaces(addresses)
	if err != nil {
		return err
	}

	entries := make(chan *zeroconf.ServiceEntry)
	done := make(chan error, 1)
	go func() {
		done <- zeroconf.Browse(ctx, service, domain, entries, zeroconf.SelectIPTraffic(zeroconf.IPv4), zeroconf.SelectIfaces(interfaces))
	}()
	// Browse does not close entries when socket construction fails. The completion
	// channel covers both initialization failure and joined cancellation.
	for {
		select {
		case err := <-done:
			return err
		case entry, ok := <-entries:
			if !ok {
				return <-done
			}
			info := ControllerInfo{Hostname: entry.HostName, Port: entry.Port}
			valid := true
			seenID, seenVersion := false, false
			for _, item := range entry.Text {
				key, value, ok := strings.Cut(item, "=")
				if !ok {
					continue
				}
				switch key {
				case "controller_id":
					if seenID {
						valid = false
					}
					seenID = true
					info.InstanceID = value
				case "protocol_major":
					if seenVersion {
						valid = false
					}
					seenVersion = true
					v, err := strconv.ParseUint(value, 10, 32)
					if err != nil {
						valid = false
					}
					info.ProtocolMajor = uint32(v)
				}
			}
			if valid {
				for _, ip := range entry.AddrIPv4 {
					if remoteIPv4(ip) && (info.IPv4 == "" || ip.String() < info.IPv4) {
						info.IPv4 = ip.String()
					}
				}
			}
			// Invalid entries count too: this caps the library's sent-entry cache as well
			// as application retention. The unbuffered channel bounds in-flight delivery.
			emit(info)
			if ctx.Err() != nil {
				return <-done
			}
		}
	}
}

// Advertise publishes immutable identity/address data. stop is idempotent and
// joins all owned work; cancellation also initiates shutdown.
func Advertise(ctx context.Context, info ControllerInfo) (stop func(), err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id, err := uuid.Parse(info.InstanceID)
	if err != nil || id == uuid.Nil || info.ProtocolMajor != protocol.Major || info.Port < 1 || info.Port > 65535 || info.Hostname == "" || len(info.Hostname) > 253 || !remoteIPv4(net.ParseIP(info.IPv4)) {
		return nil, fmt.Errorf("invalid controller advertisement")
	}
	addresses, err := localAddresses()
	if err != nil {
		return nil, err
	}
	selected, err := selectAddress(info.IPv4, nil, addresses)
	if err != nil {
		return nil, err
	}
	server, err := zeroconf.RegisterProxy(id.String(), service, domain, info.Port, info.Hostname, []string{info.IPv4}, []string{"controller_id=" + id.String(), "protocol_major=" + strconv.FormatUint(uint64(info.ProtocolMajor), 10)}, []net.Interface{selected.iface})
	if err != nil {
		return nil, err
	}
	return stopAdvertisement(ctx, server.Shutdown), nil
}

// The successful cancellation of AfterFunc transfers cleanup ownership to the
// caller. All other callers wait for the same completion, including after stop.
func stopAdvertisement(ctx context.Context, stop func()) func() {
	done := make(chan struct{})
	finish := func() { defer close(done); stop() }
	stopCallback := context.AfterFunc(ctx, finish)
	return func() {
		if stopCallback() {
			finish()
		}
		<-done
	}
}

// Browse every eligible IPv4 LAN, excluding interfaces that cannot send the
// selected address family. An empty SelectIfaces would restore library defaults.
func browseInterfaces(addresses []localAddress) ([]net.Interface, error) {
	var interfaces []net.Interface
	seen := make(map[int]bool)
	for _, address := range addresses {
		iface := address.iface
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagMulticast == 0 || iface.Flags&net.FlagLoopback != 0 || !remoteIPv4(address.ip) || seen[iface.Index] {
			continue
		}
		seen[iface.Index] = true
		interfaces = append(interfaces, iface)
	}
	if len(interfaces) == 0 {
		return nil, fmt.Errorf("no eligible LAN IPv4 multicast interface; use --controller-address HOST:PORT")
	}
	return interfaces, nil
}
