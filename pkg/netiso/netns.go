// Setup and Teardown shell out to Linux ip(8); they compile everywhere so
// the portable test file can reference them, but only work on Linux.
package netiso

import (
	"fmt"
	"os/exec"
)

// Net describes the network sandbox of one branch.
type Net struct {
	NetNSName    string // netns name, e.g. bvm-<run>-<i>
	VethHost     string // host-side veth
	VethGuest    string // netns-side veth
	TapName      string // tap device inside the netns
	HostIP       string // host veth address, e.g. 10.200.<i>.1
	GuestAPIAddr string // address the host dials for the guest API
	GatewayAddr  string // LLM gateway address the guest dials
	GuestIP      string // Firecracker guest address
	TapGatewayIP string // guest default gateway
}

// Setup creates the netns, tap, veth pair, and firewall for branch i of run.
// On failure it tears down whatever it created.
func Setup(run string, i int) (*Net, error) {
	n := &Net{
		NetNSName:    NetNSName(run, i),
		VethHost:     VethHostName(run, i),
		VethGuest:    VethGuestName(run, i),
		TapName:      TapName,
		HostIP:       HostIP(i),
		GuestAPIAddr: fmt.Sprintf("%s:%d", NSVethIP(i), GuestAPIPort),
		GatewayAddr:  fmt.Sprintf("%s:%d", HostIP(i), GatewayPort),
		GuestIP:      GuestIP,
		TapGatewayIP: TapGatewayIP,
	}
	ns := n.NetNSName
	steps := [][]string{
		{"ip", "netns", "add", ns},
		{"ip", "-n", ns, "link", "set", "lo", "up"},
		{"ip", "-n", ns, "tuntap", "add", "dev", TapName, "mode", "tap"},
		{"ip", "-n", ns, "addr", "add", TapGatewayIP + "/30", "dev", TapName},
		{"ip", "-n", ns, "link", "set", TapName, "up"},
		{"ip", "link", "add", n.VethHost, "type", "veth", "peer", "name", n.VethGuest},
		{"ip", "link", "set", n.VethGuest, "netns", ns},
		{"ip", "addr", "add", HostIP(i) + "/30", "dev", n.VethHost},
		{"ip", "link", "set", n.VethHost, "up"},
		{"ip", "-n", ns, "addr", "add", NSVethIP(i) + "/30", "dev", n.VethGuest},
		{"ip", "-n", ns, "link", "set", n.VethGuest, "up"},
		{"ip", "netns", "exec", ns, "sysctl", "-qw", "net.ipv4.ip_forward=1"},
	}
	for _, s := range steps {
		if err := runCmd(exec.Command(s[0], s[1:]...)); err != nil {
			_ = Teardown(run, i)
			return nil, fmt.Errorf("netiso setup %s: %w", ns, err)
		}
	}
	if err := ApplyFirewall(ns, i); err != nil {
		_ = Teardown(run, i)
		return nil, fmt.Errorf("netiso firewall %s: %w", ns, err)
	}
	return n, nil
}

// Teardown removes the netns and any leaked host veth. Idempotent.
func Teardown(run string, i int) error {
	if err := DeleteNetNS(NetNSName(run, i)); err != nil {
		return err
	}
	return DeleteLink(VethHostName(run, i))
}

// DeleteNetNS deletes a netns; a missing netns is not an error.
func DeleteNetNS(name string) error {
	if name == "" {
		return nil
	}
	if err := runCmd(exec.Command("ip", "netns", "delete", name)); err != nil && !isMissing(err) {
		return err
	}
	return nil
}

// DeleteLink deletes a host link; a missing link is not an error.
func DeleteLink(name string) error {
	if name == "" {
		return nil
	}
	if err := runCmd(exec.Command("ip", "link", "delete", name)); err != nil && !isMissing(err) {
		return err
	}
	return nil
}

// RunInNS builds `ip netns exec <ns> argv...`.
func RunInNS(nsName string, argv []string) *exec.Cmd {
	args := append([]string{"netns", "exec", nsName}, argv...)
	return exec.Command("ip", args...)
}
