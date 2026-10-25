package poolbench

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"

	discoveryv1 "k8s.io/api/discovery/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// maxMetricsBytes bounds what the service reads from one node agent.
const maxMetricsBytes = 16 << 20

// NodeAgentShares reads a pool's NFS counters from every node agent, through
// the agents' metrics Service, as the command-line test does.
type NodeAgentShares struct {
	Client    client.Client
	Namespace string
	Service   string
	// HTTP trusts the agents' certificate (the service CA) and names the
	// Service in its TLS server name.
	HTTP *http.Client
	// TokenFile holds the service account token the agents check.
	TokenFile string
}

// Totals adds up the reads and writes of the pool, and the seconds they took,
// over every node agent. It fails if any agent cannot be read: a partial sum
// would make the share look idle.
func (n *NodeAgentShares) Totals(ctx context.Context, pool string) (ops, seconds float64, err error) {
	slices := &discoveryv1.EndpointSliceList{}
	if err := n.Client.List(ctx, slices, client.InNamespace(n.Namespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: n.Service}); err != nil {
		return 0, 0, fmt.Errorf("list the node agents: %w", err)
	}
	token := ""
	if n.TokenFile != "" {
		raw, err := os.ReadFile(n.TokenFile)
		if err != nil {
			return 0, 0, fmt.Errorf("read the token: %w", err)
		}
		token = strings.TrimSpace(string(raw))
	}
	agents := 0
	for _, s := range slices.Items {
		if len(s.Ports) == 0 || s.Ports[0].Port == nil {
			continue
		}
		port := strconv.Itoa(int(*s.Ports[0].Port))
		for _, ep := range s.Endpoints {
			for _, addr := range ep.Addresses {
				o, sec, err := n.scrape(ctx, "https://"+net.JoinHostPort(addr, port)+"/metrics", token, pool)
				if err != nil {
					return 0, 0, fmt.Errorf("node agent %s: %w", addr, err)
				}
				ops, seconds, agents = ops+o, seconds+sec, agents+1
			}
		}
	}
	if agents == 0 {
		return 0, 0, errors.New("no node agent found")
	}
	return ops, seconds, nil
}

func (n *NodeAgentShares) scrape(ctx context.Context, url, token, pool string) (float64, float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, 0, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := n.HTTP.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	ops, secs := parseShareTotals(io.LimitReader(resp.Body, maxMetricsBytes), pool)
	return ops, secs, nil
}

// parseShareTotals reads metrics text and adds up the read and write
// operations of the pool, and their seconds. Metadata calls ("other") are left
// out: they are not disk I/O.
func parseShareTotals(r io.Reader, pool string) (ops, seconds float64) {
	want := `pool="` + pool + `"`
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		isOps := strings.HasPrefix(line, "vpc_file_pool_share_nfs_operations_total{")
		isSecs := strings.HasPrefix(line, "vpc_file_pool_share_nfs_execute_seconds_total{")
		if !isOps && !isSecs {
			continue
		}
		labels, value, ok := strings.Cut(line[strings.Index(line, "{"):], "} ")
		if !ok || !strings.Contains(labels+",", want+",") {
			continue
		}
		if !strings.Contains(labels, `op="read"`) && !strings.Contains(labels, `op="write"`) {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			continue
		}
		if isOps {
			ops += v
		} else {
			seconds += v
		}
	}
	return ops, seconds
}
