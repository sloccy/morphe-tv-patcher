package adb

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"
)

// Wireless debugging advertises itself on the LAN. Asking for that is how the TV's current port
// is learned in milliseconds; the alternative is scanning tens of thousands of ports for it.
const (
	// ServiceConnect is the service a TV offers while Wireless debugging is on.
	ServiceConnect = "_adb-tls-connect._tcp.local"
	// ServicePairing is the service it offers only while its pairing dialog is open.
	ServicePairing = "_adb-tls-pairing._tcp.local"
)

const (
	mdnsGroup   = "224.0.0.251:5353"
	mdnsRetries = 2
	// qClassINWithUnicast asks responders to answer this query straight back to the port it came
	// from (the top bit of the class). That saves having to hold 5353, which a host-network
	// container may well share with something else.
	qClassINWithUnicast = 0x8001
	typeA               = 1
	typePTR             = 12
	typeTXT             = 16
	typeSRV             = 33
)

// Lookup asks the LAN who offers a service and returns, for each IPv4 address that answered,
// the port it answered with. It is one short burst of queries: no listener is left running, and
// nothing is cached, because the port it is asked about is the thing that keeps changing.
func Lookup(ctx context.Context, service string, timeout time.Duration) (map[string]int, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		return nil, fmt.Errorf("mdns: %w", err)
	}
	defer func() { _ = conn.Close() }()
	group, err := net.ResolveUDPAddr("udp4", mdnsGroup)
	if err != nil {
		return nil, fmt.Errorf("mdns: %w", err)
	}
	q, err := question(service)
	if err != nil {
		return nil, err
	}

	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("mdns: %w", err)
	}
	for range mdnsRetries { // one lost packet should not cost us a port scan
		if _, err := conn.WriteToUDP(q, group); err != nil {
			return nil, fmt.Errorf("mdns: %w", err)
		}
	}

	found := map[string]int{}
	buf := make([]byte, 9000)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil { // the deadline, which is how this always ends
			if len(found) > 0 {
				return found, nil
			}
			var timeoutErr net.Error
			if errors.As(err, &timeoutErr) && timeoutErr.Timeout() {
				return found, nil
			}
			return found, fmt.Errorf("mdns: %w", err)
		}
		ports, addrs := records(buf[:n])
		for target, port := range ports {
			// Prefer the A record the responder sent; fall back to whoever sent the packet,
			// because some responders leave the address out of a unicast answer.
			if ip, ok := addrs[target]; ok {
				found[ip] = port
			} else if from != nil {
				found[from.IP.String()] = port
			}
		}
	}
}

// question builds a standard one-question query packet for a service's PTR records.
func question(service string) ([]byte, error) {
	name, err := encodeName(service)
	if err != nil {
		return nil, err
	}
	pkt := make([]byte, 12, 12+len(name)+4)
	binary.BigEndian.PutUint16(pkt[4:], 1) // one question, everything else zero
	pkt = append(pkt, name...)
	pkt = binary.BigEndian.AppendUint16(pkt, typePTR)
	return binary.BigEndian.AppendUint16(pkt, qClassINWithUnicast), nil
}

func encodeName(name string) ([]byte, error) {
	out := make([]byte, 0, len(name)+2)
	for name != "" {
		label := name
		if i := indexByte(name, '.'); i >= 0 {
			label, name = name[:i], name[i+1:]
		} else {
			name = ""
		}
		if label == "" {
			continue
		}
		if len(label) > 63 {
			return nil, fmt.Errorf("mdns: label too long in %q", name)
		}
		out = append(out, byte(len(label))) //nolint:gosec // the line above bounds it to 63
		out = append(out, label...)
	}
	return append(out, 0), nil
}

func indexByte(s string, b byte) int {
	for i := range len(s) {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// records pulls what we need out of a response: the port of every SRV record (by its target
// name) and the address of every A record. A responder puts the PTR in the answers and the
// SRV, TXT and A in the additional records, so both sections are read and the question is
// not checked: an unsolicited announcement is just as good an answer.
//
// The packet comes off the LAN, so every read is bounded and a malformed one simply ends the
// parse with whatever was understood so far.
func records(pkt []byte) (ports map[string]int, addrs map[string]string) {
	ports, addrs = map[string]int{}, map[string]string{}
	if len(pkt) < 12 {
		return ports, addrs
	}
	counts := [4]int{}
	for i := range counts {
		counts[i] = int(binary.BigEndian.Uint16(pkt[4+2*i:]))
	}
	off := 12
	for range counts[0] { // questions: a name then four bytes
		var ok bool
		if _, off, ok = readName(pkt, off); !ok || off+4 > len(pkt) {
			return ports, addrs
		}
		off += 4
	}
	for range counts[1] + counts[2] + counts[3] {
		_, next, ok := readName(pkt, off)
		if !ok || next+10 > len(pkt) {
			return ports, addrs
		}
		rtype := binary.BigEndian.Uint16(pkt[next:])
		length := int(binary.BigEndian.Uint16(pkt[next+8:]))
		data := next + 10
		if data+length > len(pkt) {
			return ports, addrs
		}
		switch rtype {
		case typeSRV:
			// priority, weight, port, then the target name
			if length >= 8 {
				if target, _, ok := readName(pkt, data+6); ok {
					ports[target] = int(binary.BigEndian.Uint16(pkt[data+4:]))
				}
			}
		case typeA:
			if length == 4 {
				if owner, _, ok := readName(pkt, off); ok {
					addrs[owner] = net.IP(pkt[data : data+4]).String()
				}
			}
		case typePTR, typeTXT: // nothing here that says which port to use
		}
		off = data + length
	}
	return ports, addrs
}

// readName reads a name, following the compression pointers that make one packet's names refer
// to each other. It returns the name, the offset just past it, and whether it made sense.
func readName(pkt []byte, off int) (name string, next int, ok bool) {
	const maxJumps = 16
	out := make([]byte, 0, 64)
	jumps, after := 0, -1
	for {
		if off < 0 || off >= len(pkt) {
			return "", 0, false
		}
		n := int(pkt[off])
		switch {
		case n == 0:
			off++
			if after >= 0 {
				off = after
			}
			return string(out), off, true
		case n&0xc0 == 0xc0: // a pointer to a name earlier in the packet
			if off+1 >= len(pkt) {
				return "", 0, false
			}
			jumps++
			if jumps > maxJumps {
				return "", 0, false
			}
			if after < 0 {
				after = off + 2
			}
			off = (n&0x3f)<<8 | int(pkt[off+1])
		case n&0xc0 != 0: // a label can only be those two things
			return "", 0, false
		default:
			if off+1+n > len(pkt) {
				return "", 0, false
			}
			if len(out) > 0 {
				out = append(out, '.')
			}
			out = append(out, pkt[off+1:off+1+n]...)
			off += 1 + n
		}
	}
}
