package main

import (
	"net"
	"slices"
	"strconv"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/trypando/pando/internal/core/source"
)

// sourceGuard is what a repository fetch may never connect to beyond the
// fixed classes (source/guard.go): Pando's own listening port, and its
// database — every host the connection string names, fallbacks included.
// A connection string that does not parse guards the port alone; Pando would
// not have reached its database with it to get this far.
func sourceGuard(listenAddr, databaseURL string) source.Guard {
	var g source.Guard
	if _, port, err := net.SplitHostPort(listenAddr); err == nil {
		if p, err := strconv.ParseUint(port, 10, 16); err == nil && p != 0 {
			g.OwnPorts = append(g.OwnPorts, uint16(p))
		}
	}
	cfg, err := pgconn.ParseConfig(databaseURL)
	if err != nil {
		return g
	}
	add := func(host string, port uint16) {
		// A Unix socket is a path, not an address a fetch could dial.
		if host == "" || host[0] == '/' {
			return
		}
		// pgconn lists a host twice when TLS is optional, once each way.
		hp := source.HostPort{Host: host, Port: port}
		if !slices.Contains(g.Database, hp) {
			g.Database = append(g.Database, hp)
		}
	}
	add(cfg.Host, cfg.Port)
	for _, f := range cfg.Fallbacks {
		add(f.Host, f.Port)
	}
	return g
}
