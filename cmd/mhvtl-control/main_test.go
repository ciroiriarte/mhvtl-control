package main

import "testing"

func TestIsLoopbackHost(t *testing.T) {
	cases := map[string]bool{
		"":          false, // ":8080" binds the wildcard, not loopback
		"localhost": true,
		"127.0.0.1": true,
		"127.0.0.5": true,
		"::1":       true,
		"0.0.0.0":   false,
		"10.2.0.1":  false,
		"::":        false,
	}
	for host, want := range cases {
		if got := isLoopbackHost(host); got != want {
			t.Errorf("isLoopbackHost(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestBindRefused(t *testing.T) {
	cases := []struct {
		name     string
		host     string
		token    string
		insecure bool
		want     bool
	}{
		{"non-loopback no token no insecure", "0.0.0.0", "", false, true},
		{"non-loopback with token", "0.0.0.0", "tok", false, false},
		{"non-loopback with insecure", "0.0.0.0", "", true, false},
		{"loopback no token", "127.0.0.1", "", false, false},
		{"wildcard empty host no token refused", "", "", false, true},
		{"wildcard empty host with token", "", "tok", false, false},
		{"wildcard empty host with insecure", "", "", true, false},
		{"localhost no token", "localhost", "", false, false},
	}
	for _, c := range cases {
		if got := bindRefused(c.host, c.token, c.insecure); got != c.want {
			t.Errorf("%s: bindRefused(%q,%q,%v) = %v, want %v", c.name, c.host, c.token, c.insecure, got, c.want)
		}
	}
}

func TestParseChangerFlag(t *testing.T) {
	m := map[int]string{}
	for _, v := range []string{"10=/dev/sg3", "30=/dev/sg7"} {
		if err := parseChangerFlag(m, v); err != nil {
			t.Fatalf("parseChangerFlag(%q): %v", v, err)
		}
	}
	if m[10] != "/dev/sg3" || m[30] != "/dev/sg7" || len(m) != 2 {
		t.Errorf("overrides = %v", m)
	}
	for _, bad := range []string{"/dev/sg3", "10=", "x=/dev/sg3", "-1=/dev/sg3"} {
		if err := parseChangerFlag(map[int]string{}, bad); err == nil {
			t.Errorf("parseChangerFlag(%q) accepted", bad)
		}
	}
}
