//go:build linux

package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"syscall"
)

const (
	ifaAddress = 1
	ifaLocal   = 2
	rtaOIF     = 4
	rtaGateway = 5
	rtTableMain = 254
	rtProtBoot  = 3
	rtNUnicast  = 1
)

// setupNetwork brings up lo and eth0, assigns 172.16.0.2/30 to eth0, and
// installs the default route via 172.16.0.1 using AF_NETLINK directly so
// minimal Docker images without iproute2 work out of the box.
func (a *agent) setupNetwork() error {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW, syscall.NETLINK_ROUTE)
	if err != nil {
		return fmt.Errorf("open netlink socket: %w", err)
	}
	defer syscall.Close(fd)

	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return fmt.Errorf("bind netlink socket: %w", err)
	}

	lo, err := net.InterfaceByName("lo")
	if err != nil {
		return fmt.Errorf("lookup lo: %w", err)
	}
	eth0, err := net.InterfaceByName("eth0")
	if err != nil {
		return fmt.Errorf("lookup eth0: %w", err)
	}

	var seq uint32 = 1
	if err := nlSetLinkUp(fd, &seq, lo.Index); err != nil {
		return fmt.Errorf("link lo up: %w", err)
	}
	if err := nlSetLinkUp(fd, &seq, eth0.Index); err != nil {
		return fmt.Errorf("link eth0 up: %w", err)
	}
	if err := nlAddAddrIPv4(fd, &seq, eth0.Index, net.IPv4(172, 16, 0, 2), 30); err != nil {
		return fmt.Errorf("addr eth0 %s: %w", guestCIDR, err)
	}
	if err := nlAddDefaultRoute(fd, &seq, eth0.Index, net.IPv4(172, 16, 0, 1)); err != nil {
		return fmt.Errorf("default route via %s: %w", gatewayAddr, err)
	}
	return nil
}

func nlSetLinkUp(fd int, seq *uint32, ifIndex int) error {
	// struct ifinfomsg: family(1) pad(1) type(2) index(4) flags(4) change(4) = 16 bytes.
	payload := make([]byte, 16)
	binary.LittleEndian.PutUint32(payload[4:8], uint32(ifIndex))
	binary.LittleEndian.PutUint32(payload[8:12], syscall.IFF_UP)
	binary.LittleEndian.PutUint32(payload[12:16], syscall.IFF_UP)
	return nlSendAndAck(fd, seq, syscall.RTM_NEWLINK, syscall.NLM_F_REQUEST|syscall.NLM_F_ACK, payload)
}

func nlAddAddrIPv4(fd int, seq *uint32, ifIndex int, ip net.IP, prefixLen uint8) error {
	ip4 := ip.To4()
	if ip4 == nil {
		return fmt.Errorf("not an IPv4 address: %v", ip)
	}
	// struct ifaddrmsg: family(1) prefixlen(1) flags(1) scope(1) index(4) = 8 bytes.
	payload := make([]byte, 8)
	payload[0] = syscall.AF_INET
	payload[1] = prefixLen
	binary.LittleEndian.PutUint32(payload[4:8], uint32(ifIndex))
	payload = appendRtAttr(payload, ifaLocal, ip4)
	payload = appendRtAttr(payload, ifaAddress, ip4)
	flags := uint16(syscall.NLM_F_REQUEST | syscall.NLM_F_CREATE | syscall.NLM_F_REPLACE | syscall.NLM_F_ACK)
	return nlSendAndAck(fd, seq, syscall.RTM_NEWADDR, flags, payload)
}

func nlAddDefaultRoute(fd int, seq *uint32, ifIndex int, gw net.IP) error {
	gw4 := gw.To4()
	if gw4 == nil {
		return fmt.Errorf("not an IPv4 gateway: %v", gw)
	}
	// struct rtmsg: family(1) dst_len(1) src_len(1) tos(1) table(1) protocol(1) scope(1) type(1) flags(4) = 12 bytes.
	payload := make([]byte, 12)
	payload[0] = syscall.AF_INET
	payload[4] = rtTableMain
	payload[5] = rtProtBoot
	payload[7] = rtNUnicast
	var oif [4]byte
	binary.LittleEndian.PutUint32(oif[:], uint32(ifIndex))
	payload = appendRtAttr(payload, rtaGateway, gw4)
	payload = appendRtAttr(payload, rtaOIF, oif[:])
	flags := uint16(syscall.NLM_F_REQUEST | syscall.NLM_F_CREATE | syscall.NLM_F_REPLACE | syscall.NLM_F_ACK)
	return nlSendAndAck(fd, seq, syscall.RTM_NEWROUTE, flags, payload)
}

func appendRtAttr(buf []byte, attrType uint16, val []byte) []byte {
	rawLen := 4 + len(val)
	alignedLen := (rawLen + 3) &^ 3
	start := len(buf)
	buf = append(buf, make([]byte, alignedLen)...)
	binary.LittleEndian.PutUint16(buf[start:start+2], uint16(rawLen))
	binary.LittleEndian.PutUint16(buf[start+2:start+4], attrType)
	copy(buf[start+4:], val)
	return buf
}

func nlSendAndAck(fd int, seq *uint32, msgType, flags uint16, payload []byte) error {
	s := *seq
	*seq++
	msg := make([]byte, syscall.NLMSG_HDRLEN+len(payload))
	binary.LittleEndian.PutUint32(msg[0:4], uint32(len(msg)))
	binary.LittleEndian.PutUint16(msg[4:6], msgType)
	binary.LittleEndian.PutUint16(msg[6:8], flags)
	binary.LittleEndian.PutUint32(msg[8:12], s)
	copy(msg[syscall.NLMSG_HDRLEN:], payload)

	dst := &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}
	if err := syscall.Sendto(fd, msg, 0, dst); err != nil {
		return fmt.Errorf("sendto: %w", err)
	}

	buf := make([]byte, 4096)
	for {
		n, _, err := syscall.Recvfrom(fd, buf, 0)
		if err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			return fmt.Errorf("recvfrom: %w", err)
		}
		msgs, err := syscall.ParseNetlinkMessage(buf[:n])
		if err != nil {
			return fmt.Errorf("parse netlink ack: %w", err)
		}
		for _, m := range msgs {
			if m.Header.Seq != s {
				continue
			}
			if m.Header.Type == syscall.NLMSG_DONE {
				return nil
			}
			if m.Header.Type == syscall.NLMSG_ERROR {
				if len(m.Data) < 4 {
					return fmt.Errorf("short netlink error message (%d bytes)", len(m.Data))
				}
				code := int32(binary.LittleEndian.Uint32(m.Data[:4]))
				if code == 0 {
					return nil
				}
				errno := syscall.Errno(-code)
				if errno == syscall.EEXIST {
					return nil
				}
				return errno
			}
		}
	}
}
