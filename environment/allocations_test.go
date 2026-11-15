package environment

import (
	"encoding/json"
	"testing"

	"github.com/docker/go-connections/nat"
)

func bindingPorts(t *testing.T, m nat.PortMap) map[string]int {
	t.Helper()
	out := map[string]int{}
	for p, b := range m {
		out[string(p)] = len(b)
	}
	return out
}

func TestAllocationsBindings(t *testing.T) {
	cases := []struct {
		name string
		in   Allocations
		want map[string]int
	}{
		{
			name: "no protocols defaults to tcp and udp",
			in:   Allocations{Mappings: map[string][]int{"0.0.0.0": {25565}}},
			want: map[string]int{"25565/tcp": 1, "25565/udp": 1},
		},
		{
			name: "tcp only",
			in: Allocations{
				Mappings:  map[string][]int{"0.0.0.0": {25565}},
				Protocols: map[string]map[int]string{"0.0.0.0": {25565: ProtocolTCP}},
			},
			want: map[string]int{"25565/tcp": 1},
		},
		{
			name: "udp only",
			in: Allocations{
				Mappings:  map[string][]int{"0.0.0.0": {19132}},
				Protocols: map[string]map[int]string{"0.0.0.0": {19132: ProtocolUDP}},
			},
			want: map[string]int{"19132/udp": 1},
		},
		{
			name: "explicit both",
			in: Allocations{
				Mappings:  map[string][]int{"0.0.0.0": {7777}},
				Protocols: map[string]map[int]string{"0.0.0.0": {7777: ProtocolBoth}},
			},
			want: map[string]int{"7777/tcp": 1, "7777/udp": 1},
		},
		{
			name: "protocol is matched case-insensitively",
			in: Allocations{
				Mappings:  map[string][]int{"0.0.0.0": {7777}},
				Protocols: map[string]map[int]string{"0.0.0.0": {7777: "TCP"}},
			},
			want: map[string]int{"7777/tcp": 1},
		},
		{
			name: "unknown protocol falls back to tcp and udp",
			in: Allocations{
				Mappings:  map[string][]int{"0.0.0.0": {7777}},
				Protocols: map[string]map[int]string{"0.0.0.0": {7777: "sctp"}},
			},
			want: map[string]int{"7777/tcp": 1, "7777/udp": 1},
		},
		{
			name: "protocol only applies to its own ip and port",
			in: Allocations{
				Mappings: map[string][]int{
					"1.1.1.1": {8000, 8001},
					"2.2.2.2": {8000},
				},
				Protocols: map[string]map[int]string{"1.1.1.1": {8000: ProtocolUDP}},
			},
			want: map[string]int{
				"8000/udp": 2, // 1.1.1.1 (udp only) and 2.2.2.2 (default: both)
				"8000/tcp": 1, // 2.2.2.2 only
				"8001/tcp": 1,
				"8001/udp": 1,
			},
		},
		{
			name: "same port on two ips accumulates bindings",
			in: Allocations{
				Mappings: map[string][]int{
					"1.1.1.1": {8000},
					"2.2.2.2": {8000},
				},
				Protocols: map[string]map[int]string{
					"1.1.1.1": {8000: ProtocolTCP},
					"2.2.2.2": {8000: ProtocolTCP},
				},
			},
			want: map[string]int{"8000/tcp": 2},
		},
		{
			name: "invalid ports are skipped",
			in:   Allocations{Mappings: map[string][]int{"0.0.0.0": {0, 70000, 80}}},
			want: map[string]int{"80/tcp": 1, "80/udp": 1},
		},
		{
			name: "empty mapping produces nothing",
			in:   Allocations{Mappings: map[string][]int{"": {}}},
			want: map[string]int{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := bindingPorts(t, tc.in.Bindings())
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestAllocationsBindingsHostPort(t *testing.T) {
	a := Allocations{
		Mappings:  map[string][]int{"10.0.0.5": {25565}},
		Protocols: map[string]map[int]string{"10.0.0.5": {25565: ProtocolUDP}},
	}
	b := a.Bindings()["25565/udp"]
	if len(b) != 1 || b[0].HostIP != "10.0.0.5" || b[0].HostPort != "25565" {
		t.Fatalf("unexpected binding: %+v", b)
	}
	if _, ok := a.Bindings()["25565/tcp"]; ok {
		t.Fatal("tcp binding must not exist for a udp-only allocation")
	}
}

func TestAllocationsUnmarshal(t *testing.T) {
	t.Run("legacy payload without protocols", func(t *testing.T) {
		var a Allocations
		raw := `{"force_outgoing_ip":false,"default":{"ip":"0.0.0.0","port":1},"mappings":{"0.0.0.0":[1]}}`
		if err := json.Unmarshal([]byte(raw), &a); err != nil {
			t.Fatal(err)
		}
		if a.Protocols != nil {
			t.Fatalf("expected nil protocols, got %v", a.Protocols)
		}
		if got := bindingPorts(t, a.Bindings()); len(got) != 2 {
			t.Fatalf("legacy payload must still open tcp and udp, got %v", got)
		}
	})

	t.Run("payload with protocols", func(t *testing.T) {
		var a Allocations
		raw := `{"mappings":{"0.0.0.0":[25565,19132]},"protocols":{"0.0.0.0":{"25565":"tcp","19132":"udp"}}}`
		if err := json.Unmarshal([]byte(raw), &a); err != nil {
			t.Fatal(err)
		}
		got := bindingPorts(t, a.Bindings())
		want := map[string]int{"25565/tcp": 1, "19132/udp": 1}
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for k := range want {
			if got[k] != 1 {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
	})
}
