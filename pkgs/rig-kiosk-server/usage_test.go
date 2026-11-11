package main

import "testing"

func TestParseMeminfo(t *testing.T) {
	u := parseMeminfo("MemTotal:       16777216 kB\nMemFree:          100000 kB\nMemAvailable:    4194304 kB\n")
	if u == nil {
		t.Fatal("no usage parsed")
	}
	if u.Total != 16<<30 || u.Used != 12<<30 {
		t.Fatalf("got used=%d total=%d", u.Used, u.Total)
	}
	if u.Text != "12.0 GiB / 16.0 GiB · 75%" || u.Full {
		t.Fatalf("got %q full=%v", u.Text, u.Full)
	}

	if parseMeminfo("MemTotal: 1024 kB\n") != nil {
		t.Fatal("expected nil without MemAvailable")
	}
}

func TestFull(t *testing.T) {
	if !newUsage(95, 100).Full || newUsage(50, 100).Full {
		t.Fatal("full threshold wrong")
	}
	if newUsage(0, 0) != nil {
		t.Fatal("expected nil for zero total")
	}
}

func TestFormatBytes(t *testing.T) {
	for n, want := range map[uint64]string{
		512:       "512 B",
		1536:      "1.5 KiB",
		500 << 30: "500 GiB",
		2 << 40:   "2.0 TiB",
	} {
		if got := formatBytes(n); got != want {
			t.Errorf("formatBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestDiskUsageRoot(t *testing.T) {
	if diskUsage("/") == nil {
		t.Fatal("could not stat /")
	}
}
