package privacy

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/projectbluefin/bluefin-report/internal/snapshot"
)

func str(v string) json.RawMessage { b, _ := json.Marshal(v); return b }

func metric(name, object, value string) snapshot.Metric {
	return snapshot.Metric{Name: name, Object: object, Value: str(value)}
}

func sample() snapshot.Snapshot {
	return snapshot.Snapshot{
		System: snapshot.Report{MediaType: "application/vnd.io.systemd.report", Metrics: []snapshot.Metric{
			metric("io.systemd.Basic.Hostname", "", "orca-desk"),
			metric("io.systemd.Basic.MachineID", "", "0123456789abcdef0123456789abcdef"),
			metric("io.systemd.Basic.BootID", "", "fedcba9876543210fedcba9876543210"),
			metric("io.systemd.Basic.MachineInfo.LOCATION", "", "Jorge's office"),
			metric("io.systemd.Basic.SMBIOS.SystemSerialNumber", "", "PF3ABCD9"),
			metric("io.systemd.Basic.SMBIOS.SystemUUID", "", "4c4c4544-0042-3510-8051-b4c04f4e5a31"),
			metric("io.systemd.Basic.SMBIOS.BaseBoardSerial", "", "L1HF16M00XY"),
			metric("io.systemd.Basic.SMBIOS.ChassisSerialNumber", "", "CHS99887766"),
			metric("io.systemd.Basic.SMBIOS.BaseBoardAssetTag", "", "ASSET-0042"),
			metric("io.systemd.Basic.SMBIOS.ChassisAssetTagNumber", "", "TAG-7788"),
			metric("io.systemd.Basic.SMBIOS.SystemProductName", "", "Laptop 13 (AMD Ryzen 7040Series)"),
			metric("io.systemd.Report.SignPlain.PublicKey", "", "-----BEGIN PUBLIC KEY-----\nMCowBQYDK2VwAyEAuniquekey\n-----END PUBLIC KEY-----"),
			metric("io.systemd.Basic.KernelVersion", "", "7.2.11-ogc4"),
			metric("io.systemd.Basic.OSRelease.IMAGE_ID", "", "dakota-gaming"),
			metric("io.systemd.Files.os-release", "", "ID=bluefin"),
			metric("io.systemd.Manager.UnitActiveState", "var-home-jorge-vault.mount", "failed"),
			metric("io.systemd.Manager.UnitActiveState", "orca-desk-backup.service", "failed"),
			metric("io.systemd.Manager.UnitActiveState", "rpc-statd.service", "failed"),
			metric("io.systemd.Manager.UnitActiveState", "getty@tty1.service", "active"),
			metric("io.systemd.Manager.StateChangeTimestamp", "sshd.service", "00:37:42"),
			{Name: "io.systemd.Network.Address", Object: "wlp1s0", Value: str("192.168.7.21"),
				Fields: map[string]any{"mac": "3c:22:fb:01:aa:9e", "v6": "fe80::3e22:fbff:fe01:aa9e", "n": 1.0}},
			metric("io.systemd.Network.Owner", "", "contact jorge@example.org about /var/home/jorge/notes"),
		}},
		User: snapshot.Report{Metrics: []snapshot.Metric{
			metric("io.systemd.Manager.UnitActiveState", "app-orca\\x2ddesk.scope", "active"),
		}},
	}
}

func TestBuildRemovesIdentifiersAndKeepsDiagnostics(t *testing.T) {
	p, err := Build(sample(), Options{Username: "jorge", IncludeUser: true})
	if err != nil {
		t.Fatal(err)
	}
	out := string(p.JSON)
	for _, secret := range []string{"orca-desk", "x2ddesk", "0123456789abcdef", "fedcba98", "Jorge's office", "jorge",
		"192.168.7.21", "3c:22:fb", "fe80::", "example.org", "ID=bluefin",
		"PF3ABCD9", "4c4c4544", "L1HF16M00XY", "CHS99887766", "ASSET-0042", "TAG-7788", "uniquekey"} {
		if strings.Contains(out, secret) {
			t.Errorf("payload still contains %q", secret)
		}
	}
	for _, keep := range []string{"7.2.11-ogc4", "dakota-gaming", "Laptop 13 (AMD Ryzen 7040Series)", "rpc-statd.service", "getty@tty1.service", "00:37:42", "wlp1s0", "var-home-[username]-vault.mount"} {
		if !strings.Contains(out, keep) {
			t.Errorf("payload lost diagnostic value %q", keep)
		}
	}
	var doc map[string]any
	if err := json.Unmarshal(p.JSON, &doc); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}
	if doc["user"] == nil {
		t.Error("user scope requested but missing")
	}
	if p.Removed["identifying metric"] != 12 || p.Removed["MAC address"] != 1 || p.Removed["IPv4 address"] != 1 ||
		p.Removed["IPv6 address"] != 1 || p.Removed["email"] != 1 || p.Removed["home path"] != 1 {
		t.Errorf("Removed = %v", p.Removed)
	}
}

func TestBuildRedactsCompressedIPv6AndKeepsDiagnostics(t *testing.T) {
	for _, tt := range []struct {
		name  string
		input string
		want  string
		rule  string
	}{
		{"mapped IPv4", "::ffff:192.0.2.128", "[ipv4]", "IPv4 address"},
		{"trailing compression", "2001:db8::", "[ipv6]", "IPv6 address"},
		{"leading compression", "::1", "[ipv6]", "IPv6 address"},
		{"unspecified", "::", "[ipv6]", "IPv6 address"},
		{"bracketed", "[2001:db8::]", "[[ipv6]]", "IPv6 address"},
		{"bracketed loopback", "[::1]:443", "[[ipv6]]:443", "IPv6 address"},
		{"full IPv6", "2001:db8:85a3:1:2:8a2e:370:7334", "[ipv6]", "IPv6 address"},
		{"IPv6 final numeric group", "2001:db8::443", "[ipv6]", "IPv6 address"},
		{"sentence punctuation", "(2001:db8::).", "([ipv6]).", "IPv6 address"},
		{"mapped sentence punctuation", "(::ffff:192.0.2.128).", "([ipv4]).", "IPv4 address"},
		{"leading punctuation", "...::1", "...[ipv6]", "IPv6 address"},
		{"IPv4", "192.0.2.128", "[ipv4]", "IPv4 address"},
		{"IPv4 punctuation", ":192.0.2.128.", ":[ipv4].", "IPv4 address"},
		{"IPv4 port", "192.0.2.128:443", "[ipv4]:443", "IPv4 address"},
		{"IPv4 peer diagnostic", "peer 192.0.2.128:443 closed", "peer [ipv4]:443 closed", "IPv4 address"},
		{"IPv4 port punctuation", "peer (192.0.2.128:443). closed", "peer ([ipv4]:443). closed", "IPv4 address"},
		{"bracketed IPv4 port", "[192.0.2.128]:443", "[[ipv4]]:443", "IPv4 address"},
		{"IPv4 after label", "ip:192.0.2.128", "ip:[ipv4]", "IPv4 address"},
		{"IPv6 after label", "host:fe80::1", "host:[ipv6]", "IPv6 address"},
		{"IPv4 port after label", "ip:192.0.2.128:443", "ip:[ipv4]:443", "IPv4 address"},
		{"version", "7.2.11-ogc4", "7.2.11-ogc4", ""},
		{"time", "00:37:42", "00:37:42", ""},
		{"C++ string", "std::string", "std::string", ""},
		{"Rust panic", "core::panicking", "core::panicking", ""},
		{"namespace", "foo::bar", "foo::bar", ""},
		{"GTK type", "Gtk::Widget", "Gtk::Widget", ""},
		{"backtrace", "std::string\ncore::panicking\nfoo::bar\nGtk::Widget", "std::string\ncore::panicking\nfoo::bar\nGtk::Widget", ""},
		{"underscore prefix", "_::1", "_::1", ""},
		{"underscore suffix", "::1_", "::1_", ""},
		{"Unicode letter prefix", "λ::1", "λ::1", ""},
		{"Unicode letter suffix", "::1界", "::1界", ""},
		{"Unicode mark prefix", "e\u0301::1", "e\u0301::1", ""},
		{"Unicode mark suffix", "::1\u0301", "::1\u0301", ""},
		{"Unicode number prefix", "١::1", "١::1", ""},
		{"Unicode number suffix", "::1Ⅷ", "::1Ⅷ", ""},
		{"Unicode connector prefix", "‿::1", "‿::1", ""},
		{"Unicode connector suffix", "::1‿", "::1‿", ""},
		{"Unicode joiner prefix", "\u200d::1", "\u200d::1", ""},
		{"Unicode joiner suffix", "::1\u200c", "::1\u200c", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := metric("io.systemd.Network.Address", tt.input, tt.input)
			m.Fields = map[string]any{"address": tt.input}
			s := snapshot.Snapshot{System: snapshot.Report{Metrics: []snapshot.Metric{m}}}
			p, err := Build(s, Options{})
			if err != nil {
				t.Fatal(err)
			}
			var doc document
			if err := json.Unmarshal(p.JSON, &doc); err != nil {
				t.Fatal(err)
			}
			if len(doc.System.Metrics) != 1 {
				t.Fatalf("got %d metrics, want 1", len(doc.System.Metrics))
			}
			got := doc.System.Metrics[0]
			var value string
			if err := json.Unmarshal(got.Value, &value); err != nil {
				t.Fatal(err)
			}
			if got.Object != tt.want || value != tt.want || got.Fields["address"] != tt.want {
				t.Errorf("scrubbed object = %q, value = %q, field = %q; want %q",
					got.Object, value, got.Fields["address"], tt.want)
			}
			if tt.rule == "" {
				if len(p.Removed) != 0 {
					t.Errorf("diagnostic was counted as removed: %v", p.Removed)
				}
			} else {
				if strings.Contains(string(p.JSON), tt.input) {
					t.Errorf("exported payload still contains %q", tt.input)
				}
				if len(p.Removed) != 1 || p.Removed[tt.rule] != 3 {
					t.Errorf("Removed = %v, want %q: 3", p.Removed, tt.rule)
				}
			}
		})
	}
}

func TestBuildRedactsMixedAddressesAndPreservesBacktrace(t *testing.T) {
	input := "std::string peer 192.0.2.128:443 closed\ncore::panicking [::1]:8443 ::ffff:192.0.2.128 foo::bar Gtk::Widget"
	want := "std::string peer [ipv4]:443 closed\ncore::panicking [[ipv6]]:8443 [ipv4] foo::bar Gtk::Widget"
	m := metric("io.systemd.Journal.Message", input, input)
	m.Fields = map[string]any{"message": input}
	p, err := Build(snapshot.Snapshot{System: snapshot.Report{Metrics: []snapshot.Metric{m}}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var doc document
	if err := json.Unmarshal(p.JSON, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.System.Metrics) != 1 {
		t.Fatalf("got %d metrics, want 1", len(doc.System.Metrics))
	}
	got := doc.System.Metrics[0]
	var value string
	if err := json.Unmarshal(got.Value, &value); err != nil {
		t.Fatal(err)
	}
	if got.Object != want || value != want || got.Fields["message"] != want {
		t.Errorf("scrubbed object = %q, value = %q, field = %q; want %q",
			got.Object, value, got.Fields["message"], want)
	}
	if strings.Contains(string(p.JSON), "192.0.2.128") || strings.Contains(string(p.JSON), "::1") {
		t.Error("exported payload still contains an address")
	}
	if len(p.Removed) != 2 || p.Removed["IPv4 address"] != 6 || p.Removed["IPv6 address"] != 3 {
		t.Errorf("Removed = %v, want IPv4 address: 6, IPv6 address: 3", p.Removed)
	}
}

func TestBuildUserScopeAndFilesAreOptIn(t *testing.T) {
	p, err := Build(sample(), Options{Username: "jorge", KeepFiles: []string{"io.systemd.Files.os-release"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(p.JSON), `"user"`) {
		t.Error("user scope attached without opt-in")
	}
	if !strings.Contains(string(p.JSON), "ID=bluefin") {
		t.Error("opted-in file family was dropped")
	}
}

func TestBuildBudgetOmitsWholeFamiliesAndStaysValid(t *testing.T) {
	full, _ := Build(sample(), Options{Username: "jorge"})
	limit := len(full.JSON) - 50

	p, err := Build(sample(), Options{Username: "jorge", MaxBytes: limit})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.JSON) > limit {
		t.Errorf("payload %d bytes exceeds budget %d", len(p.JSON), limit)
	}
	var doc struct {
		System          snapshot.Report
		OmittedFamilies []string
	}
	if err := json.Unmarshal(p.JSON, &doc); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}
	if len(p.Omitted) == 0 || p.Omitted[0] != "io.systemd.Manager.UnitActiveState" || doc.OmittedFamilies[0] != p.Omitted[0] {
		t.Errorf("Omitted = %v (largest family should go first, and be recorded in the document)", p.Omitted)
	}
	for _, m := range doc.System.Metrics {
		if m.Name == p.Omitted[0] {
			t.Error("omitted family still partially present")
		}
	}

	if _, err := Build(sample(), Options{MaxBytes: 10}); err == nil {
		t.Error("expected error when nothing fits")
	}
}

func TestBuildMasksVirtualInterfaces(t *testing.T) {
	s := snapshot.Snapshot{System: snapshot.Report{Metrics: []snapshot.Metric{
		metric("io.systemd.Network.Address", "tailscale0", "up"),
		metric("io.systemd.Network.Address", "wg0", "up"),
		metric("io.systemd.Network.Address", "docker0", "up"),
		{Name: "io.systemd.Network.Link", Object: "wlp1s0", Value: str("up"), Fields: map[string]any{"peer": "veth1234abc"}},
		metric("io.systemd.Manager.UnitActiveState", "docker0", "active"), // not a network metric
	}}}
	p, err := Build(s, Options{})
	if err != nil {
		t.Fatal(err)
	}
	out := string(p.JSON)
	for _, name := range []string{"tailscale0", "wg0", "veth1234abc"} {
		if strings.Contains(out, name) {
			t.Errorf("payload still contains %q", name)
		}
	}
	if !strings.Contains(out, "wlp1s0") || p.Removed["virtual interface"] != 4 {
		t.Errorf("Removed = %v, out = %s", p.Removed, out)
	}
}

func TestExtrasAreRedacted(t *testing.T) {
	journal := "Oct 31 10:00:01 orca-desk systemd[1]: jorge-backup failed for jorge from 10.0.0.5\n" +
		"Oct 31 10:00:02 orca-desk kernel: eth0 3c:22:fb:01:aa:9e unit app-orca\\x2ddesk.scope /home/jorge/x"
	p, err := Build(sample(), Options{Username: "jorge", Extras: map[string]string{"bootErrors": journal}})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{string(p.JSON), p.Extras["bootErrors"]} {
		for _, secret := range []string{"orca-desk", "x2ddesk", "jorge", "10.0.0.5", "3c:22:fb"} {
			if strings.Contains(s, secret) {
				t.Errorf("extras still contain %q: %s", secret, s)
			}
		}
	}
	if !strings.Contains(string(p.JSON), `"extras": {`) || !strings.Contains(p.Extras["bootErrors"], "[hostname] kernel") {
		t.Errorf("extras missing from payload: %s", p.Extras["bootErrors"])
	}
	if p, _ := Build(sample(), Options{}); p.Extras != nil || strings.Contains(string(p.JSON), "extras") {
		t.Error("no opt-in must mean no extras")
	}
}

// The artifact is the payload inside a ```json fence; together they must stay under 1 MiB.
func TestDefaultBudgetLeavesRoomForFence(t *testing.T) {
	s := sample()
	for i := range 20000 {
		s.System.Metrics = append(s.System.Metrics, metric("io.systemd.Big", strings.Repeat("x", 40), strings.Repeat("y", i%80)))
	}
	p, err := Build(s, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if n := len("```json\n") + len(p.JSON) + len("\n```\n"); len(p.Omitted) == 0 || n >= 1<<20 {
		t.Fatalf("fenced artifact is %d bytes, omitted %v", n, p.Omitted)
	}
}

// When the system scope is empty there is no io.systemd.Basic.Hostname metric,
// so no hostname literal was ever registered and a host mentioned only in a
// user metric or opt-in extra would leak into the attachment. os.Hostname()
// supplies the machine identity so it is still scrubbed.
func TestBuildScrubbersHostnameFromEmptySystemScope(t *testing.T) {
	host, err := os.Hostname()
	if err != nil || host == "" {
		t.Skipf("no hostname available: %v", err)
	}
	if len(host) < 3 {
		t.Skipf("hostname %q too short for the 3-char literal floor", host)
	}
	s := snapshot.Snapshot{
		System: snapshot.Report{MediaType: "application/vnd.io.systemd.report", Metrics: []snapshot.Metric{}},
		User: snapshot.Report{Metrics: []snapshot.Metric{
			metric("io.systemd.Manager.UnitActiveState", host+"\\x2ddesktop.scope", "active"),
		}},
	}
	p, err := Build(s, Options{Username: "jorge", IncludeUser: true,
		Extras: map[string]string{"bootErrors": "Oct 31 10:00:01 " + host + " systemd[1]: backup failed\n"}})
	if err != nil {
		t.Fatal(err)
	}
	out := string(p.JSON)
	if strings.Contains(out, host) {
		t.Errorf("payload still contains the hostname %q from an empty system scope", host)
	}
	if !strings.Contains(out, "[hostname]") {
		t.Errorf("expected hostname rule to fire, Removed = %v", p.Removed)
	}
	if p.Removed["hostname"] < 2 {
		t.Errorf("expected hostname scrubbed from user metric and extras, Removed = %v", p.Removed)
	}
}
