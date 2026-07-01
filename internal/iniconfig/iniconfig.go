// Package iniconfig provides a simple INI file parser.
package iniconfig

import (
	"net"
	"os"
	"strings"
)

// ParseFile reads an INI file and returns key-value pairs.
// Returns the map and true if the file exists, nil and false otherwise.
// Lines starting with # are treated as comments.
func ParseFile(path string) (map[string]string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	m := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		m[key] = value
	}
	return m, true
}

// CombinePort combines an address with a port if the address doesn't already have one.
// Handles IPv6 addresses with brackets correctly.
func CombinePort(addr, port string) string {
	if addr == "" || port == "" {
		return addr
	}
	if _, _, err := net.SplitHostPort(addr); err == nil {
		// Already has a port
		return addr
	}
	// Strip brackets from IPv6 to avoid double-bracketing
	if strings.HasPrefix(addr, "[") && strings.HasSuffix(addr, "]") {
		addr = addr[1 : len(addr)-1]
	}
	return net.JoinHostPort(addr, port)
}
