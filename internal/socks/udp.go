package socks

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync/atomic"
	"syscall"
)

type udpAssociation struct {
	conn     *net.UDPConn
	clientIP netip.Addr
	flowID   uint16
	endpoint atomic.Pointer[net.UDPAddr]
}

func newUDPAssociation(client net.Conn, requested target) (*udpAssociation, error) {
	remote, ok := client.RemoteAddr().(*net.TCPAddr)
	if !ok || remote == nil {
		return nil, errors.New("socks: UDP association requires a TCP client address")
	}
	clientIP, ok := netip.AddrFromSlice(remote.IP)
	if !ok {
		return nil, errBadAddress
	}
	clientIP = clientIP.Unmap()
	requestedIP, err := netip.ParseAddr(requested.Host)
	if err != nil {
		return nil, errAddressTypeUnsupported
	}
	requestedIP = requestedIP.Unmap()
	if !requestedIP.IsUnspecified() && requestedIP != clientIP {
		return nil, errBadAddress
	}
	local, ok := client.LocalAddr().(*net.TCPAddr)
	if !ok || local == nil {
		return nil, errors.New("socks: UDP association requires a TCP listener address")
	}
	network := "udp6"
	if local.IP.To4() != nil {
		network = "udp4"
	}
	udp, err := net.ListenUDP(network, &net.UDPAddr{IP: append(net.IP(nil), local.IP...), Zone: local.Zone})
	if err != nil {
		return nil, fmt.Errorf("socks: bind UDP association: %w", err)
	}
	bound, ok := udp.LocalAddr().(*net.UDPAddr)
	if !ok || bound.Port < 1 || bound.Port > 65535 {
		_ = udp.Close()
		return nil, errors.New("socks: invalid UDP association address")
	}
	association := &udpAssociation{conn: udp, clientIP: clientIP, flowID: uint16(bound.Port)}
	if requested.Port != 0 {
		association.endpoint.Store(&net.UDPAddr{IP: append(net.IP(nil), remote.IP...), Port: int(requested.Port), Zone: remote.Zone})
	}
	return association, nil
}

func (association *udpAssociation) acceptSource(address *net.UDPAddr) bool {
	if address == nil || address.Port < 1 || address.Port > 65535 {
		return false
	}
	ip, ok := netip.AddrFromSlice(address.IP)
	if !ok || ip.Unmap() != association.clientIP {
		return false
	}
	if endpoint := association.endpoint.Load(); endpoint != nil {
		return address.Port == endpoint.Port
	}
	copyAddress := &net.UDPAddr{IP: append(net.IP(nil), address.IP...), Port: address.Port, Zone: address.Zone}
	return association.endpoint.CompareAndSwap(nil, copyAddress)
}

func (association *udpAssociation) FlowID() uint16      { return association.flowID }
func (association *udpAssociation) LocalAddr() net.Addr { return association.conn.LocalAddr() }
func (association *udpAssociation) Close() error        { return association.conn.Close() }

func (association *udpAssociation) ReadPacket(packet []byte) (int, error) {
	for {
		n, _, flags, source, err := association.conn.ReadMsgUDP(packet, nil)
		if err != nil {
			return 0, fmt.Errorf("socks: read client UDP datagram: %w", err)
		}
		if flags&syscall.MSG_TRUNC == 0 && association.acceptSource(source) {
			return n, nil
		}
	}
}

func (association *udpAssociation) WritePacket(packet []byte) error {
	endpoint := association.endpoint.Load()
	if endpoint == nil {
		return nil
	}
	if _, err := association.conn.WriteToUDP(packet, endpoint); err != nil {
		return fmt.Errorf("socks: write client UDP datagram: %w", err)
	}
	return nil
}
