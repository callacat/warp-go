package tunnel

import (
	"context"
	"fmt"
	"log"
	"net"
	"time"
)

const (
	// TTL bounds for the resolution cache. The floor keeps very short TTLs from
	// making the cache useless; the ceiling keeps us from pinning a stale address.
	dohMinTTL = 5 * time.Second
	dohMaxTTL = 5 * time.Minute

	// Cache sizing. Sweeping starts once the map is large enough that the scan
	// cost is worth paying; the cap bounds memory even if every entry is live.
	dnsCacheSweepAt    = 1024
	dnsCacheMaxEntries = 8192
)

type dnsCacheEntry struct {
	ip        net.IP
	expiresAt time.Time
}

type dnsFlightResult struct {
	ip   net.IP
	err  error
	done chan struct{}
}

// resolveTarget resolves through WARP tunnel DNS
func (c *MasqueClient) resolveTarget(ctx context.Context, targetAddr string) (connectTarget string, hostHeader string, err error) {
	host, port, splitErr := net.SplitHostPort(targetAddr)
	if splitErr != nil {
		// Passing an unparseable target through unchanged used to hide a
		// malformed address until the edge cancelled the stream, with nothing in
		// the log pointing at the cause. Fail here, where the address is named.
		return "", "", fmt.Errorf("目标地址 %q 无法解析为 host:port：%w", targetAddr, splitErr)
	}
	if net.ParseIP(host) != nil {
		// Re-join rather than echoing the input: this normalises an IPv6 literal
		// to exactly one level of brackets.
		return net.JoinHostPort(host, port), "", nil
	}

	ip, dnsErr := c.resolveDNS(ctx, host)
	if dnsErr != nil {
		// resolveDNS already names the host; don't wrap it a second time.
		return "", "", dnsErr
	}

	// JoinHostPort brackets an IPv6 literal itself. Bracketing here as well
	// produced "[[2606:...]]:443", which the edge answered by cancelling the
	// stream — a latent bug that only surfaced once AAAA answers were used.
	return net.JoinHostPort(ip.String(), port), host, nil
}

// cacheResolution stores a resolution and keeps the cache from growing without
// bound. Entries were previously only ever added: expired ones were skipped on
// read but never removed, so a long run that resolves many distinct names (any
// browser workload) grew the map forever.
//
// Sweeping expired entries once the map crosses a threshold bounds it by the
// number of names resolved within one TTL window rather than by uptime. The hard
// cap is a backstop for the pathological case where that window alone is huge;
// dropping the whole map is acceptable because the cache is an optimisation, not
// a source of truth.
func (c *MasqueClient) cacheResolution(host string, ip net.IP, ttl time.Duration) {
	c.dnsCacheMu.Lock()
	defer c.dnsCacheMu.Unlock()

	if len(c.dnsCache) >= dnsCacheSweepAt {
		now := time.Now()
		for name, entry := range c.dnsCache {
			if !now.Before(entry.expiresAt) {
				delete(c.dnsCache, name)
			}
		}
		if len(c.dnsCache) >= dnsCacheMaxEntries {
			log.Printf("DNS 缓存中 %d 条仍在有效期内，整体清空", len(c.dnsCache))
			clear(c.dnsCache)
		}
	}
	c.dnsCache[host] = dnsCacheEntry{ip: ip, expiresAt: time.Now().Add(ttl)}
}

// ResolveDNS 经隧道内 DoH 解析 host（A/AAAA 并发，A 优先），返回边缘网络
// 视图可达的 IP。Android DNS 拦截服务器用它响应 TUN 内的 DNS 查询——关键：
// 只有隧道内 DoH 解析出的 IP 才是 WARP 边缘可达的（v0.5.24 Android 根因：
// 系统 DNS 解析出的 IP 与边缘网络视图不同，边缘 CONNECT 该 IP hang 到
// deadline）。返回的 IP 同时被拦截服务器记录进 IP→域名映射表，供
// NewConnectionEx 还原域名后走 DialTunnel（内部再次 DoH 解析，保证边缘可达）。
func (c *MasqueClient) ResolveDNS(ctx context.Context, host string) (net.IP, error) {
	return c.resolveDNS(ctx, host)
}

func (c *MasqueClient) resolveDNS(ctx context.Context, host string) (net.IP, error) {
	// Check cache first
	c.dnsCacheMu.RLock()
	if entry, ok := c.dnsCache[host]; ok && time.Now().Before(entry.expiresAt) {
		c.dnsCacheMu.RUnlock()
		log.Printf("WARP DNS（缓存）✓ %s -> %s", host, entry.ip)
		return entry.ip, nil
	}
	c.dnsCacheMu.RUnlock()

	// Singleflight: if another goroutine is already resolving this host, wait for it.
	c.dnsFlightMu.Lock()
	if flight, ok := c.dnsFlight[host]; ok {
		c.dnsFlightMu.Unlock()
		select {
		case <-flight.done:
			return flight.ip, flight.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	flight := &dnsFlightResult{done: make(chan struct{})}
	c.dnsFlight[host] = flight
	c.dnsFlightMu.Unlock()

	// Do the actual resolution — singleflight means only one goroutine runs this
	// for a given host at a time.
	//
	// Retry exactly once, and only when the shared connection itself was at fault:
	// dnsQuery has already retired that connection (by identity), so the second
	// attempt dials a fresh one. A DNS-level answer — NXDOMAIN, no A record, a
	// non-200 status — says nothing about the connection, and neither does this
	// query's own deadline expiring; retrying or tearing the connection down for
	// those would abort every other lookup multiplexed on it.
	var ip net.IP
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		resolved, ttl, err := c.dnsQuery(ctx, host)
		if err == nil {
			ip = resolved
			lastErr = nil
			log.Printf("WARP DNS ✓ %s -> %s（TTL %s）", host, ip, ttl)
			c.cacheResolution(host, ip, ttl)
			break
		}
		lastErr = err
		if !shouldRetryDoH(err, ctx.Err()) {
			break
		}
	}
	if ip == nil {
		lastErr = fmt.Errorf("%s 的 DNS 解析失败：%w", host, lastErr)
	}

	// Store result and wake waiters
	flight.ip = ip
	flight.err = lastErr
	close(flight.done)

	// Clean up flight entry
	c.dnsFlightMu.Lock()
	delete(c.dnsFlight, host)
	c.dnsFlightMu.Unlock()

	return ip, lastErr
}
