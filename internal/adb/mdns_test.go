package adb

import (
	"context"
	"encoding/binary"
	"net"
	"testing"
	"time"
)

// reply builds a response packet the way a real responder does: the PTR in the answers, and the
// SRV and A in the additional records, with the names compressed into each other.
func reply(t *testing.T, service, instance, host string, port int, ip string) []byte {
	t.Helper()
	pkt := binary.BigEndian.AppendUint16(nil, 0)     // transaction id
	pkt = binary.BigEndian.AppendUint16(pkt, 0x8400) // a response, authoritative
	pkt = binary.BigEndian.AppendUint16(pkt, 0)      // no questions echoed back
	pkt = binary.BigEndian.AppendUint16(pkt, 1)      // one answer
	pkt = binary.BigEndian.AppendUint16(pkt, 0)      // no authority records
	pkt = binary.BigEndian.AppendUint16(pkt, 2)      // two additional records

	svc, err := encodeName(service)
	if err != nil {
		t.Fatal(err)
	}
	serviceAt := len(pkt) // where the service name lands, for the pointers below
	full, err := encodeName(instance + "." + service)
	if err != nil {
		t.Fatal(err)
	}
	hostName, err := encodeName(host)
	if err != nil {
		t.Fatal(err)
	}
	// the instance name, written out once and then pointed at
	inst := append([]byte{}, full[:len(full)-len(svc)]...)
	inst = binary.BigEndian.AppendUint16(inst, 0xc000|uint16(serviceAt)) //nolint:gosec // a test packet

	rec := func(name []byte, rtype uint16, data []byte) {
		pkt = append(pkt, name...)
		pkt = binary.BigEndian.AppendUint16(pkt, rtype)
		pkt = binary.BigEndian.AppendUint16(pkt, 1)                 // class IN
		pkt = binary.BigEndian.AppendUint32(pkt, 120)               // ttl
		pkt = binary.BigEndian.AppendUint16(pkt, uint16(len(data))) //nolint:gosec // a test packet
		pkt = append(pkt, data...)
	}
	instAt := serviceAt + len(svc) + 10
	rec(svc, typePTR, inst)
	srv := binary.BigEndian.AppendUint16(nil, 0)           // priority
	srv = binary.BigEndian.AppendUint16(srv, 0)            // weight
	srv = binary.BigEndian.AppendUint16(srv, uint16(port)) //nolint:gosec // a test packet
	srv = append(srv, hostName...)
	rec(binary.BigEndian.AppendUint16(nil, 0xc000|uint16(instAt)), typeSRV, srv) //nolint:gosec // a test packet
	rec(hostName, typeA, net.ParseIP(ip).To4())
	return pkt
}

func TestRecordsReadsTheSRVPortAndTheAddress(t *testing.T) {
	pkt := reply(t, ServiceConnect, "adb-ABC123-xyz", "tv.local", 41234, "10.0.0.7")
	ports, addrs := records(pkt)
	if len(ports) != 1 {
		t.Fatalf("one SRV: %v", ports)
	}
	for target, port := range ports {
		if port != 41234 || addrs[target] != "10.0.0.7" {
			t.Fatalf("target %q port %d addrs %v", target, port, addrs)
		}
	}
}

func TestRecordsSurvivesRubbish(t *testing.T) {
	good := reply(t, ServiceConnect, "adb-ABC123-xyz", "tv.local", 41234, "10.0.0.7")
	cases := map[string][]byte{
		"empty":             {},
		"header only":       good[:12],
		"cut mid-name":      good[:20],
		"pointer to itself": {0, 0, 0x84, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0xc0, 12, 0, 1, 0, 1, 0, 0, 0, 1, 0, 0},
		"reserved label":    {0, 0, 0x84, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0x40, 1, 'x', 0, 0, 1, 0, 1, 0, 0, 0, 1, 0, 0},
		"silly counts":      {0, 0, 0x84, 0, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
	}
	for name, pkt := range cases {
		t.Run(name, func(t *testing.T) {
			if ports, addrs := records(pkt); len(ports) > 0 { // must not panic, hang or invent a port
				t.Errorf("%v %v", ports, addrs)
			}
		})
	}
	// A packet cut short after a whole record keeps what it already understood: the port is the
	// part that matters, and Lookup falls back to the address the packet came from.
	ports, addrs := records(good[:len(good)-3])
	if len(ports) != 1 || len(addrs) != 0 {
		t.Errorf("%v %v", ports, addrs)
	}
}

func TestReadNameFollowsPointersButNotForever(t *testing.T) {
	// two pointers that point at each other
	pkt := []byte{0xc0, 2, 0xc0, 0}
	if _, _, ok := readName(pkt, 0); ok {
		t.Error("a pointer loop must be refused")
	}
	if _, _, ok := readName([]byte{3, 'a', 'b'}, 0); ok {
		t.Error("a label running past the packet must be refused")
	}
	name, next, ok := readName([]byte{2, 'h', 'i', 0, 9}, 0)
	if !ok || name != "hi" || next != 4 {
		t.Errorf("%q %d %v", name, next, ok)
	}
}

func TestQuestionAsksForThePTRWithTheUnicastBit(t *testing.T) {
	q, err := question(ServiceConnect)
	if err != nil {
		t.Fatal(err)
	}
	if binary.BigEndian.Uint16(q[4:]) != 1 {
		t.Errorf("one question: %v", q[:12])
	}
	name, next, ok := readName(q, 12)
	if !ok || name != ServiceConnect {
		t.Fatalf("%q %v", name, ok)
	}
	if binary.BigEndian.Uint16(q[next:]) != typePTR || binary.BigEndian.Uint16(q[next+2:]) != qClassINWithUnicast {
		t.Errorf("type/class %x %x", binary.BigEndian.Uint16(q[next:]), binary.BigEndian.Uint16(q[next+2:]))
	}
}

func TestLookupReturnsNothingWhenNobodyAnswers(t *testing.T) {
	// Nothing on the LAN is asked to answer in a test, so this proves only that the query goes
	// out and the wait ends on its own rather than hanging or failing.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	found, err := Lookup(ctx, ServiceConnect, 50*time.Millisecond)
	if err != nil {
		t.Skipf("no multicast here: %v", err) // a sandboxed builder has no LAN to ask
	}
	for ip, port := range found { // a real TV on the developer's LAN may actually reply
		if port <= 0 || net.ParseIP(ip) == nil {
			t.Errorf("%s -> %d", ip, port)
		}
	}
}
