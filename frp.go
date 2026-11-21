// Package caddyfrp는 frps admin API를 조회해 frp 터널이 online일 때만 매치되는 request matcher를 제공한다.
//
// 빌드: GOOS=linux GOARCH=arm64 xcaddy build --with github.com/dgdsingen/caddy-frps=.
package caddyfrp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

func init() {
	caddy.RegisterModule(MatchFrpOnline{})
}

var (
	_ caddy.Module                      = (*MatchFrpOnline)(nil)
	_ caddy.Provisioner                 = (*MatchFrpOnline)(nil)
	_ caddy.Validator                   = (*MatchFrpOnline)(nil)
	_ caddy.CleanerUpper                = (*MatchFrpOnline)(nil)
	_ caddyhttp.RequestMatcherWithError = (*MatchFrpOnline)(nil)
	_ caddyfile.Unmarshaler             = (*MatchFrpOnline)(nil)
)

const (
	defaultRefresh = 10 * time.Second
	defaultTimeout = 3 * time.Second

	// v2 API(/api/v2/proxies)는 페이지네이션이라 전체 목록을 한 번에 주는 v1을 쓴다.
	proxyListPath = "/api/proxy/http"

	// offline 프록시도 목록에 남으므로(conf는 null) status로 걸러야 한다.
	statusOnline = "online"
)

// MatchFrpOnline은 {sub_domain}.{sub_domain_host} 로 라우팅되는 frp 프록시가 online일 때만 매치된다.
type MatchFrpOnline struct {
	// frps dashboard 주소. scheme 필수 (예: http://frp.example.com:7500)
	AdminAPI string `json:"admin_api,omitempty"`
	// dashboard basic auth. admin_password와 함께 설정하거나 둘 다 비운다.
	AdminUser     string `json:"admin_user,omitempty"`
	AdminPassword string `json:"admin_password,omitempty"`
	// frps의 subDomainHost 설정값
	SubDomainHost string `json:"sub_domain_host,omitempty"`
	// 조회할 subdomain. placeholder 사용 가능 (예: {vars.svc}--{vars.ns})
	SubDomain string `json:"sub_domain,omitempty"`
	// 캐시 갱신 주기. 기본 10s
	Refresh caddy.Duration `json:"refresh,omitempty"`
	// admin API 조회 타임아웃. 기본 3s
	Timeout caddy.Duration `json:"timeout,omitempty"`

	cache         *proxyCache
	cacheKey      string
	subDomainHost string // 소문자로 정규화한 SubDomainHost
}

func (MatchFrpOnline) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.matchers.frp_online",
		New: func() caddy.Module { return new(MatchFrpOnline) },
	}
}

// UnmarshalCaddyfile은 frp_online 블록을 파싱한다. 모든 옵션은 인자를 정확히 하나 받는다.
func (m *MatchFrpOnline) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		if d.NextArg() {
			return d.ArgErr()
		}
		for d.NextBlock(0) {
			option := d.Val()
			var value string
			if !d.Args(&value) {
				return d.ArgErr()
			}
			if d.NextArg() {
				return d.ArgErr()
			}

			switch option {
			case "admin_api":
				m.AdminAPI = value
			case "admin_user":
				m.AdminUser = value
			case "admin_password":
				m.AdminPassword = value
			case "sub_domain_host":
				m.SubDomainHost = value
			case "sub_domain":
				m.SubDomain = value
			case "refresh", "timeout":
				parsed, err := caddy.ParseDuration(value)
				if err != nil {
					return d.Errf("invalid %s duration '%s': %v", option, value, err)
				}
				if option == "refresh" {
					m.Refresh = caddy.Duration(parsed)
				} else {
					m.Timeout = caddy.Duration(parsed)
				}
			default:
				return d.Errf("unrecognized frp_online option '%s'", option)
			}
		}
	}
	return nil
}

// Validate는 부작용이 없다. Provision이 먼저 호출하고 Caddy가 다시 호출해도 된다.
func (m *MatchFrpOnline) Validate() error {
	if m.AdminAPI == "" {
		return fmt.Errorf("admin_api is required")
	}
	parsed, err := url.Parse(m.AdminAPI)
	if err != nil {
		return fmt.Errorf("invalid admin_api: %v", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("admin_api scheme must be http or https, got '%s'", parsed.Scheme)
	}
	if parsed.Host == "" {
		return fmt.Errorf("admin_api must include a host")
	}
	// URL의 자격증명은 fetch 실패 로그(endpoint)에 그대로 찍힌다.
	if parsed.User != nil {
		return fmt.Errorf("admin_api must not contain userinfo; use admin_user/admin_password instead")
	}
	// 하나만 설정되면 frps가 항상 401을 돌려 진단 없이 영구 fail-closed가 된다.
	if (m.AdminUser == "") != (m.AdminPassword == "") {
		return fmt.Errorf("admin_user and admin_password must be set together")
	}
	if m.SubDomainHost == "" {
		return fmt.Errorf("sub_domain_host is required")
	}
	if m.SubDomain == "" {
		return fmt.Errorf("sub_domain is required")
	}
	if m.Refresh <= 0 {
		return fmt.Errorf("refresh must be positive")
	}
	if m.Timeout <= 0 {
		return fmt.Errorf("timeout must be positive")
	}
	return nil
}

// proxyListResponse는 frps 응답 중 판정에 필요한 필드만 담는다.
type proxyListResponse struct {
	Proxies []struct {
		Status string `json:"status"`
		Conf   struct {
			SubDomain     string   `json:"subdomain"`
			CustomDomains []string `json:"customDomains"`
		} `json:"conf"`
	} `json:"proxies"`
}

// snapshot은 online 프록시의 도메인 집합이다. 모든 값은 소문자다.
// sub_domain_host는 조회 시점에 조합하므로 host가 다른 matcher끼리도 캐시를 공유한다.
type snapshot struct {
	subDomains    map[string]struct{}
	customDomains map[string]struct{}
}

func (s *snapshot) has(subDomain, fqdn string) bool {
	if _, ok := s.subDomains[subDomain]; ok {
		return true
	}
	_, ok := s.customDomains[fqdn]
	return ok
}

type frpClient struct {
	endpoint string
	user     string
	password string
	http     *http.Client
}

func newFrpClient(adminAPI, user, password string, timeout time.Duration) *frpClient {
	return &frpClient{
		endpoint: strings.TrimSuffix(adminAPI, "/") + proxyListPath,
		user:     user,
		password: password,
		http:     &http.Client{Timeout: timeout},
	}
}

func (c *frpClient) fetch(ctx context.Context) (*snapshot, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint, nil)
	if err != nil {
		return nil, err
	}
	if c.user != "" {
		req.SetBasicAuth(c.user, c.password)
	}

	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("frps admin api returned %s", res.Status)
	}

	var parsed proxyListResponse
	if err := json.NewDecoder(res.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decoding frps admin api response: %v", err)
	}

	snap := &snapshot{
		subDomains:    make(map[string]struct{}),
		customDomains: make(map[string]struct{}),
	}
	// frps vhost 라우터가 도메인을 소문자로 비교하므로 똑같이 맞춘다.
	for _, proxy := range parsed.Proxies {
		if proxy.Status != statusOnline {
			continue
		}
		if proxy.Conf.SubDomain != "" {
			snap.subDomains[strings.ToLower(proxy.Conf.SubDomain)] = struct{}{}
		}
		for _, domain := range proxy.Conf.CustomDomains {
			snap.customDomains[strings.ToLower(domain)] = struct{}{}
		}
	}
	return snap, nil
}

// proxyCache는 stale-while-revalidate로 스냅샷을 유지한다.
// 갱신에 실패하면 스냅샷을 버려 fail-closed가 된다. frps가 죽으면 터널도 같이 끊기므로
// 옛 스냅샷으로 online 판정을 유지하면 죽은 터널로 계속 보내 502만 늘어난다.
type proxyCache struct {
	client  *frpClient
	refresh time.Duration
	logger  *zap.Logger
	now     func() time.Time

	mu          sync.RWMutex
	snap        *snapshot // nil이면 판정 불가
	lastAttempt time.Time // 성공/실패와 무관한 마지막 조회 시각

	fetching atomic.Bool
}

func newProxyCache(client *frpClient, refresh time.Duration, logger *zap.Logger) *proxyCache {
	return &proxyCache{client: client, refresh: refresh, logger: logger, now: time.Now}
}

// get은 요청 경로에서 호출되므로 블로킹하지 않는다.
// refresh가 지났으면 goroutine 하나만 띄워 갱신하고, 현재 스냅샷을 바로 반환한다.
func (c *proxyCache) get() *snapshot {
	c.mu.RLock()
	snap, lastAttempt := c.snap, c.lastAttempt
	c.mu.RUnlock()

	if c.now().Sub(lastAttempt) >= c.refresh && c.fetching.CompareAndSwap(false, true) {
		go func() {
			defer c.fetching.Store(false)
			_ = c.fetch(context.Background())
		}()
	}
	return snap
}

// fetch는 frps를 조회해 스냅샷을 교체한다. 실패하면 스냅샷은 nil이 된다.
func (c *proxyCache) fetch(ctx context.Context) error {
	snap, err := c.client.fetch(ctx)

	c.mu.Lock()
	c.snap = snap
	c.lastAttempt = c.now()
	c.mu.Unlock()

	if err != nil {
		c.logger.Warn("failed to fetch frps proxy list",
			zap.String("endpoint", c.client.endpoint),
			zap.Error(err))
	}
	return err
}

// caches는 접속 설정이 같은 matcher끼리 캐시를 공유하고, config reload 사이에도 유지한다.
var caches = caddy.NewUsagePool()

type cacheEntry struct{ *proxyCache }

func (cacheEntry) Destruct() error { return nil }

func (m *MatchFrpOnline) Provision(ctx caddy.Context) error {
	if m.Refresh == 0 {
		m.Refresh = caddy.Duration(defaultRefresh)
	}
	if m.Timeout == 0 {
		m.Timeout = caddy.Duration(defaultTimeout)
	}
	// Caddy는 Validate를 Provision 뒤에 부른다. 그 전에 아래 로그가 잘못된 admin_api를 찍지 않게 먼저 검증한다.
	if err := m.Validate(); err != nil {
		return err
	}
	m.subDomainHost = strings.ToLower(m.SubDomainHost)

	// 비밀번호는 평문 대신 digest로 키에 넣는다. 비밀번호만 바뀐 reload도 새 캐시(새 자격증명)를 쓰게 된다.
	// %q는 구분자(|)가 섞인 값끼리 키가 충돌하지 않게 한다.
	passwordDigest := sha256.Sum256([]byte(m.AdminPassword))
	cacheKey := fmt.Sprintf("%q|%q|%s|%s|%s",
		m.AdminAPI, m.AdminUser, time.Duration(m.Refresh), time.Duration(m.Timeout), hex.EncodeToString(passwordDigest[:]))

	logger := ctx.Logger()
	value, loaded, err := caches.LoadOrNew(cacheKey, func() (caddy.Destructor, error) {
		client := newFrpClient(m.AdminAPI, m.AdminUser, m.AdminPassword, time.Duration(m.Timeout))
		return cacheEntry{newProxyCache(client, time.Duration(m.Refresh), logger)}, nil
	})
	if err != nil {
		return err
	}
	m.cacheKey = cacheKey
	m.cache = value.(cacheEntry).proxyCache

	// 새 캐시면 동기 조회로 채워 기동 직후 폴백 구간을 없앤다. 실패해도 기동은 막지 않는다.
	if !loaded {
		fetchCtx, cancel := context.WithTimeout(ctx.Context, time.Duration(m.Timeout))
		defer cancel()
		_ = m.cache.fetch(fetchCtx)
	}

	logger.Info("frp_online matcher provisioned",
		zap.String("admin_api", m.AdminAPI),
		zap.Duration("refresh", time.Duration(m.Refresh)),
		zap.Duration("timeout", time.Duration(m.Timeout)))
	return nil
}

func (m *MatchFrpOnline) Cleanup() error {
	if m.cacheKey == "" {
		return nil
	}
	// Delete는 에러여도 refcount를 이미 줄였으므로 먼저 비워 이중 차감을 막는다.
	key := m.cacheKey
	m.cacheKey = ""
	_, err := caches.Delete(key)
	return err
}

// MatchWithError는 에러를 반환하지 않는다.
// 에러를 반환하면 Caddy가 error 핸들러로 보내 기존 upstream으로의 폴백이 깨진다.
func (m *MatchFrpOnline) MatchWithError(r *http.Request) (bool, error) {
	repl := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)
	subDomain := strings.ToLower(repl.ReplaceAll(m.SubDomain, ""))
	if subDomain == "" {
		return false, nil
	}

	snap := m.cache.get()
	if snap == nil {
		return false, nil
	}
	return snap.has(subDomain, subDomain+"."+m.subDomainHost), nil
}
