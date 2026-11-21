// Package netiso builds the per-branch network sandbox: a network
// namespace holding the Firecracker tap device, a veth pair to the host,
// and a policy-drop nftables ruleset applied inside the namespace.
package netiso

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

const (
	// TapName is the tap device inside every branch netns.
	TapName = "tap0"
	// TapGatewayIP is the tap address the guest uses as its gateway.
	TapGatewayIP = "172.16.0.1"
	// GuestIP is the Firecracker guest address.
	GuestIP = "172.16.0.2"
	// GuestAPIPort is the guest agent HTTP port.
	GuestAPIPort = 8080
	// GatewayPort is the host LLM gateway port the guest may reach.
	GatewayPort = 18080
	// maxIfName is the Linux interface name limit (IFNAMSIZ - 1).
	maxIfName = 15
)

// NetNSName returns the namespace name for branch i of run.
func NetNSName(run string, i int) string { return fmt.Sprintf("bvm-%s-%d", run, i) }

// VethHostName returns the host-side veth name, shortening the run part
// so the name fits the 15 byte kernel limit.
func VethHostName(run string, i int) string { return ifName("bvmh", run, i) }

// VethGuestName returns the netns-side veth name.
func VethGuestName(run string, i int) string { return ifName("bvmg", run, i) }

func ifName(prefix, run string, i int) string {
	suffix := fmt.Sprintf("-%d", i)
	room := maxIfName - len(prefix) - 1 - len(suffix)
	if room < 0 {
		room = 0
	}
	if len(run) > room {
		run = run[:room]
	}
	return prefix + "-" + run + suffix
}

// HostIP returns the host veth address (no mask) for branch i.
func HostIP(i int) string { return fmt.Sprintf("10.200.%d.1", i) }

// NSVethIP returns the netns veth address (no mask) for branch i.
func NSVethIP(i int) string { return fmt.Sprintf("10.200.%d.2", i) }

// TableName returns the nftables table name for branch i.
func TableName(i int) string { return fmt.Sprintf("bvm_%d", i) }

// Ruleset returns the complete nft -f document for branch i. It is a pure
// function so it can be unit tested on any OS.
func Ruleset(i int) string {
	t := TableName(i)
	veth := fmt.Sprintf("10.200.%d.0/30", i)
	hostIP, nsIP := HostIP(i), NSVethIP(i)
	return fmt.Sprintf(`# branchvm firewall for branch %[1]d, applied inside its netns.
# Create then delete so the following add is atomic and idempotent.
table inet %[2]s
delete table inet %[2]s

table inet %[2]s {
	chain prerouting {
		type nat hook prerouting priority dstnat; policy accept;
		# Host reaches the guest API only through the veth address.
		ip daddr %[4]s tcp dport %[6]d dnat ip to %[7]s:%[6]d
	}

	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		# DNAT'd API flow: guest sees the tap gateway as the client.
		ct status dnat oifname "%[8]s" ip daddr %[7]s tcp dport %[6]d masquerade
		# Guest to LLM gateway: host sees the netns veth as the client.
		ip saddr %[7]s ip daddr %[3]s tcp dport %[10]d masquerade
	}

	chain forward {
		type filter hook forward priority filter; policy drop;
		# No IPv6 at all.
		meta nfproto ipv6 drop
		# DNS and DNS-over-TLS exfil, any destination.
		meta l4proto { tcp, udp } th dport { 53, 853 } drop
		# Metadata service and loopback nets.
		ip daddr 169.254.0.0/16 drop
		ip daddr 127.0.0.0/8 drop
		# RFC1918 except the two branch /30s.
		ip daddr { 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16 } ip daddr != { %[5]s, 172.16.0.0/30 } drop
		ct state invalid drop
		ct state established,related accept
		# Forward leg of the API DNAT.
		ct status dnat iifname "%[9]s" oifname "%[8]s" ip daddr %[7]s tcp dport %[6]d ct state new accept
		# Guest to LLM gateway.
		iifname "%[8]s" oifname "%[9]s" ip saddr %[7]s ip daddr %[3]s tcp dport %[10]d ct state new accept
	}

	chain output {
		type filter hook output priority filter; policy drop;
		oifname "lo" accept
		meta l4proto { tcp, udp } th dport { 53, 853 } drop
		ip daddr 169.254.0.0/16 drop
		ct state established,related accept
		# Tap gateway to guest for the DNAT return path.
		oifname "%[8]s" ip saddr %[11]s ip daddr %[7]s accept
	}
}
`, i, t, hostIP, nsIP, veth, GuestAPIPort, GuestIP, TapName, VethGuestNamePlaceholder, GatewayPort, TapGatewayIP)
}

// VethGuestNamePlaceholder is the iifname pattern for the netns veth end.
// The netns holds exactly one veth, so a wildcard keeps Ruleset run-agnostic.
const VethGuestNamePlaceholder = "bvmg-*"

// ApplyFirewall loads the ruleset for branch i inside nsName atomically.
func ApplyFirewall(nsName string, i int) error {
	cmd := exec.Command("ip", "netns", "exec", nsName, "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(Ruleset(i))
	return runCmd(cmd)
}

// ClearFirewall deletes the branch table; a missing table or netns is not an error.
func ClearFirewall(nsName string, i int) error {
	err := runCmd(exec.Command("ip", "netns", "exec", nsName, "nft", "delete", "table", "inet", TableName(i)))
	if err != nil && isMissing(err) {
		return nil
	}
	return err
}

func runCmd(cmd *exec.Cmd) error {
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w: %s", strings.Join(cmd.Args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func isMissing(err error) bool {
	s := err.Error()
	for _, m := range []string{"No such file", "Cannot find device", "does not exist", "not found", "Invalid argument"} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}
