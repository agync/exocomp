package api

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// AddressFile is where a running backend records its bound address, inside the
// data dir. A saved tofu plan carries the backend address it was made against,
// so plan and apply have to reach the same one; the data dir is the single
// place both processes already agree on.
const AddressFile = "state-backend.addr"

// probeTimeout bounds the health check against a possibly stale address.
const probeTimeout = time.Second

// WriteAddress records the bound address. The file outlives the process on
// purpose: it is how the next run finds the same port, and a stale entry is
// caught by Reachable rather than by a lock.
func WriteAddress(path, addr string) error {
	if err := os.WriteFile(path, []byte(addr+"\n"), 0o600); err != nil {
		return fmt.Errorf("api: write address file: %w", err)
	}
	return nil
}

// ReadAddress returns the last recorded address, or an empty string when none
// was recorded.
func ReadAddress(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("api: read address file: %w", err)
	}
	return strings.TrimSpace(string(raw)), nil
}

// PortOf returns the port of a recorded address, or 0 when it has none.
func PortOf(addr string) int {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return 0
	}
	return n
}

// Reachable reports whether a state backend answers at addr.
func Reachable(ctx context.Context, addr string) bool {
	if addr == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/healthz", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return false
	}
	return resp.StatusCode == http.StatusOK && strings.TrimSpace(string(body)) == "ok"
}
