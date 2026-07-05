package db

import "testing"

func TestHostPort(t *testing.T) {
	cases := map[string]string{
		"postgres://u@localhost:5432/app":   "localhost:5432",
		"postgres://u@db.example.com/app":   "db.example.com:5432", // default port
		"postgresql://u:p@10.0.0.1:6000/db": "10.0.0.1:6000",
		"mysql://u@localhost:3307/app":      "localhost:3307",
		"mysql://u@localhost/app":           "localhost:3306", // default port
		"sqlite:///tmp/a.db":                "",               // no network host
		"./notes.db":                        "",               // bare path
		"postgres:///var/run/pg/db":         "",               // unix socket, no host
	}
	for in, want := range cases {
		if got := HostPort(in); got != want {
			t.Errorf("HostPort(%q) = %q, want %q", in, got, want)
		}
	}
}
