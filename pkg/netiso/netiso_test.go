package netiso

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

func TestRuleset(t *testing.T) {
	r := Ruleset(3)
	must := []string{
		"table inet bvm_3",
		"chain forward {\n\t\ttype filter hook forward priority filter; policy drop;",
		"chain output {\n\t\ttype filter hook output priority filter; policy drop;",
		"ip daddr 10.200.3.2 tcp dport 8080 dnat ip to 172.16.0.2:8080",
		"ip daddr 10.200.3.1 tcp dport 18080 ct state new accept",
		"ip daddr 169.254.0.0/16 drop",
		"th dport { 53, 853 } drop",
		"ct state established,related accept",
		"ip daddr != { 10.200.3.0/30, 172.16.0.0/30 } drop",
	}
	for _, m := range must {
		if !strings.Contains(r, m) {
			t.Errorf("ruleset missing %q", m)
		}
	}
	if strings.Contains(r, "0.0.0.0/0") {
		t.Error("ruleset must not reference 0.0.0.0/0")
	}
	// Drops must precede the first accept in forward.
	fwd := r[strings.Index(r, "chain forward"):]
	if strings.Index(fwd, "dport { 53, 853 } drop") > strings.Index(fwd, "accept") {
		t.Error("DNS drop must precede accepts")
	}
	if strings.ContainsRune(r, 0x2014) {
		t.Error("em dash in ruleset")
	}
}

func TestIfNameFits(t *testing.T) {
	n := VethHostName("averyveryverylongrunid", 12)
	if len(n) > 15 || !strings.HasPrefix(n, "bvmh-") || !strings.HasSuffix(n, "-12") {
		t.Fatalf("bad name %q", n)
	}
}

func TestIntegrationNetNS(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("netns integration test requires linux")
	}
	if os.Geteuid() != 0 {
		t.Skip("netns integration test requires root")
	}
	if os.Getenv("BRANCHVM_NETNS_TEST") != "1" {
		t.Skip("set BRANCHVM_NETNS_TEST=1 to run netns integration test")
	}
	integration(t)
}

func sh(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v: %s", args, err, out)
	}
	return string(out)
}

func assertClean(t *testing.T) {
	t.Helper()
	if strings.Contains(sh(t, "ip", "netns", "list"), "bvm-") {
		t.Error("bvm- netns remains")
	}
	if strings.Contains(sh(t, "ip", "-o", "link"), "bvmh-") {
		t.Error("bvmh- link remains")
	}
}

func integration(t *testing.T) {
	const run = "it"
	n, err := Setup(run, 7)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if !strings.Contains(sh(t, "ip", "netns", "list"), n.NetNSName) {
		t.Fatal("netns missing")
	}
	sh(t, "ip", "link", "show", n.VethHost)
	sh(t, "ip", "netns", "exec", n.NetNSName, "nft", "list", "table", "inet", TableName(7))
	if err := Teardown(run, 7); err != nil {
		t.Fatalf("teardown 1: %v", err)
	}
	if err := Teardown(run, 7); err != nil {
		t.Fatalf("teardown 2: %v", err)
	}
	assertClean(t)
}
