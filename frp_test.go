package caddyfrp

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddytest"
	_ "github.com/caddyserver/caddy/v2/modules/standard"
	"go.uber.org/zap"
)

func TestUnmarshalCaddyfile(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
		want    MatchFrpOnline
	}{
		{
			name: "모든 옵션 지정",
			input: `frp_online {
	admin_api http://frp.example.com:7500
	admin_user admin
	admin_password secret
	sub_domain_host frp.example.com
	sub_domain {vars.svc}--{vars.ns}
	refresh 30s
	timeout 1s
}`,
			want: MatchFrpOnline{
				AdminAPI:      "http://frp.example.com:7500",
				AdminUser:     "admin",
				AdminPassword: "secret",
				SubDomainHost: "frp.example.com",
				SubDomain:     "{vars.svc}--{vars.ns}",
				Refresh:       caddy.Duration(30 * time.Second),
				Timeout:       caddy.Duration(time.Second),
			},
		},
		{
			name: "선택 옵션 생략",
			input: `frp_online {
	admin_api http://frp.example.com:7500
	sub_domain_host frp.example.com
	sub_domain api--test
}`,
			want: MatchFrpOnline{
				AdminAPI:      "http://frp.example.com:7500",
				SubDomainHost: "frp.example.com",
				SubDomain:     "api--test",
			},
		},
		{
			name: "알 수 없는 옵션",
			input: `frp_online {
	nope 1
}`,
			wantErr: true,
		},
		{
			name: "잘못된 duration",
			input: `frp_online {
	refresh abc
}`,
			wantErr: true,
		},
		{
			name: "인자 누락",
			input: `frp_online {
	admin_api
}`,
			wantErr: true,
		},
		{
			name: "옵션 인자 초과",
			input: `frp_online {
	admin_api http://a http://b
}`,
			wantErr: true,
		},
		{
			name:    "인라인 인자 불허",
			input:   `frp_online extra`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var m MatchFrpOnline
			err := m.UnmarshalCaddyfile(caddyfile.NewTestDispenser(tt.input))
			if tt.wantErr {
				if err == nil {
					t.Fatal("에러를 기대했지만 nil을 받았다")
				}
				return
			}
			if err != nil {
				t.Fatalf("예상치 못한 에러: %v", err)
			}
			if m != tt.want {
				t.Errorf("파싱 결과 불일치\n got: %+v\nwant: %+v", m, tt.want)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	valid := MatchFrpOnline{
		AdminAPI:      "http://frp.example.com:7500",
		SubDomainHost: "frp.example.com",
		SubDomain:     "api--test",
		Refresh:       caddy.Duration(defaultRefresh),
		Timeout:       caddy.Duration(defaultTimeout),
	}

	tests := []struct {
		name    string
		mutate  func(*MatchFrpOnline)
		wantErr bool
	}{
		{"정상", func(*MatchFrpOnline) {}, false},
		{"admin_api 누락", func(m *MatchFrpOnline) { m.AdminAPI = "" }, true},
		{"admin_api scheme 없음", func(m *MatchFrpOnline) { m.AdminAPI = "frp.example.com:7500" }, true},
		{"admin_api host 없음", func(m *MatchFrpOnline) { m.AdminAPI = "http://" }, true},
		{"admin_api에 자격증명 포함", func(m *MatchFrpOnline) { m.AdminAPI = "http://admin:secret@frp.example.com:7500" }, true},
		{"admin_user만 설정", func(m *MatchFrpOnline) { m.AdminUser = "admin" }, true},
		{"admin_password만 설정", func(m *MatchFrpOnline) { m.AdminPassword = "secret" }, true},
		{"admin_user/admin_password 둘 다 설정", func(m *MatchFrpOnline) { m.AdminUser = "admin"; m.AdminPassword = "secret" }, false},
		{"sub_domain_host 누락", func(m *MatchFrpOnline) { m.SubDomainHost = "" }, true},
		{"sub_domain 누락", func(m *MatchFrpOnline) { m.SubDomain = "" }, true},
		{"refresh 0", func(m *MatchFrpOnline) { m.Refresh = 0 }, true},
		{"timeout 음수", func(m *MatchFrpOnline) { m.Timeout = -1 }, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := valid
			tt.mutate(&m)
			err := m.Validate()
			if tt.wantErr && err == nil {
				t.Fatal("에러를 기대했지만 nil을 받았다")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("예상치 못한 에러: %v", err)
			}
		})
	}
}

// newTestFrps는 고정된 JSON을 돌려주는 가짜 frps admin API를 띄운다.
// 반환된 카운터로 실제 호출 횟수를 확인할 수 있다.
func newTestFrps(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestFrpClientFetch(t *testing.T) {
	const body = `{
		"proxies": [
			{"name": "a", "status": "online",  "conf": {"subdomain": "API--Feature-X"}},
			{"name": "b", "status": "offline", "conf": null},
				{"name": "e", "status": "offline", "conf": {"subdomain": "gone--feature-y"}},
			{"name": "c", "status": "online",  "conf": {"customDomains": ["Web.Example.com", "cdn.example.com"]}},
			{"name": "d", "status": "online",  "conf": {}}
		]
	}`

	srv, _ := newTestFrps(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != proxyListPath {
			t.Errorf("예상치 못한 경로: %s", r.URL.Path)
		}
		user, password, ok := r.BasicAuth()
		if !ok || user != "admin" || password != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, body)
	})

	client := newFrpClient(srv.URL, "admin", "secret", time.Second)
	snap, err := client.fetch(context.Background())
	if err != nil {
		t.Fatalf("예상치 못한 에러: %v", err)
	}

	if !snap.has("api--feature-x", "api--feature-x.frp.example.com") {
		t.Error("online subdomain을 찾지 못했다")
	}
	if snap.has("gone--feature-y", "gone--feature-y.frp.example.com") {
		t.Error("offline 프록시가 스냅샷에 들어갔다")
	}
	if !snap.has("nothing", "web.example.com") {
		t.Error("customDomains를 찾지 못했다")
	}
	if snap.has("", "") {
		t.Error("빈 값이 매치되었다")
	}
}

func TestFrpClientFetchErrors(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"401", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) }},
		{"500", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) }},
		{"깨진 JSON", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "{not json") }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := newTestFrps(t, tt.handler)
			client := newFrpClient(srv.URL, "", "", time.Second)
			if _, err := client.fetch(context.Background()); err == nil {
				t.Fatal("에러를 기대했지만 nil을 받았다")
			}
		})
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("시간 안에 조건을 만족하지 못했다: %s", what)
}

// newTestCache는 online subdomain 하나를 돌려주는 가짜 frps에 붙은 캐시를 만든다.
// failing을 true로 바꾸면 이후 조회가 500으로 실패한다.
func newTestCache(t *testing.T, refresh time.Duration) (*proxyCache, *atomic.Int64, *atomic.Bool) {
	t.Helper()
	var failing atomic.Bool
	srv, calls := newTestFrps(t, func(w http.ResponseWriter, r *http.Request) {
		if failing.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		// 동시 요청이 겹치도록 약간 지연시킨다.
		time.Sleep(10 * time.Millisecond)
		fmt.Fprint(w, `{"proxies":[{"status":"online","conf":{"subdomain":"api--feature-x"}}]}`)
	})

	cache := newProxyCache(newFrpClient(srv.URL, "", "", time.Second), refresh, zap.NewNop())
	return cache, calls, &failing
}

func TestCacheServesFreshSnapshotWithoutRefetching(t *testing.T) {
	cache, calls, _ := newTestCache(t, time.Hour)

	if err := cache.fetch(context.Background()); err != nil {
		t.Fatalf("초기 조회 실패: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("초기 조회 횟수 = %d, want 1", got)
	}

	for range 10 {
		if cache.get() == nil {
			t.Fatal("스냅샷이 nil이다")
		}
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("fresh 캐시인데 재조회했다. 호출 횟수 = %d, want 1", got)
	}
}

func TestCacheServesStaleWhileRevalidating(t *testing.T) {
	cache, calls, _ := newTestCache(t, 10*time.Second)

	if err := cache.fetch(context.Background()); err != nil {
		t.Fatalf("초기 조회 실패: %v", err)
	}

	// refresh가 지난 것처럼 시계를 앞당긴다.
	base := time.Now()
	cache.now = func() time.Time { return base.Add(time.Minute) }

	// 만료 직후 요청도 기다리지 않고 기존 스냅샷으로 판정해야 한다.
	if snap := cache.get(); snap == nil {
		t.Fatal("만료 시 stale 스냅샷 대신 nil을 받았다")
	}
	waitFor(t, "백그라운드 갱신 완료", func() bool { return calls.Load() == 2 })
}

func TestCacheGoesFailClosedWhenFetchFails(t *testing.T) {
	cache, _, failing := newTestCache(t, 10*time.Second)

	if err := cache.fetch(context.Background()); err != nil {
		t.Fatalf("초기 조회 실패: %v", err)
	}

	failing.Store(true)
	if err := cache.fetch(context.Background()); err == nil {
		t.Fatal("에러를 기대했지만 nil을 받았다")
	}
	if snap := cache.get(); snap != nil {
		t.Error("조회 실패 후에도 스냅샷이 남아있다. fail-closed가 아니다")
	}

	// 복구되면 다음 갱신에서 다시 채워져야 한다.
	failing.Store(false)
	if err := cache.fetch(context.Background()); err != nil {
		t.Fatalf("복구 조회 실패: %v", err)
	}
	if snap := cache.get(); snap == nil {
		t.Error("복구 후에도 스냅샷이 nil이다")
	}
}

func TestCacheRefreshesOnlyOnceUnderConcurrency(t *testing.T) {
	cache, calls, _ := newTestCache(t, 10*time.Second)

	if err := cache.fetch(context.Background()); err != nil {
		t.Fatalf("초기 조회 실패: %v", err)
	}

	base := time.Now()
	cache.now = func() time.Time { return base.Add(time.Minute) }

	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cache.get()
		}()
	}
	wg.Wait()

	waitFor(t, "백그라운드 갱신 완료", func() bool { return calls.Load() >= 2 })
	time.Sleep(50 * time.Millisecond)
	if got := calls.Load(); got != 2 {
		t.Errorf("동시 요청에서 갱신이 중복 실행됐다. 호출 횟수 = %d, want 2", got)
	}
}

// newTestRequest는 주어진 placeholder가 채워진 요청을 만든다.
func newTestRequest(vars map[string]string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	repl := caddy.NewReplacer()
	for key, value := range vars {
		repl.Set(key, value)
	}
	return req.WithContext(context.WithValue(req.Context(), caddy.ReplacerCtxKey, repl))
}

// staticCache는 갱신을 트리거하지 않는 고정 스냅샷 캐시를 만든다.
func staticCache(snap *snapshot) *proxyCache {
	cache := newProxyCache(nil, time.Hour, zap.NewNop())
	cache.snap = snap
	cache.lastAttempt = time.Now()
	return cache
}

func TestMatchWithError(t *testing.T) {
	snap := &snapshot{
		subDomains:    map[string]struct{}{"api--feature-x": {}},
		customDomains: map[string]struct{}{"web.frp.example.com": {}},
	}

	tests := []struct {
		name      string
		subDomain string
		cache     *proxyCache
		vars      map[string]string
		want      bool
	}{
		{
			name:      "subdomain 매치",
			subDomain: "{vars.svc}--{vars.ns}",
			cache:     staticCache(snap),
			vars:      map[string]string{"vars.svc": "api", "vars.ns": "feature-x"},
			want:      true,
		},
		{
			name:      "대소문자 무시",
			subDomain: "{vars.svc}--{vars.ns}",
			cache:     staticCache(snap),
			vars:      map[string]string{"vars.svc": "API", "vars.ns": "Feature-X"},
			want:      true,
		},
		{
			name:      "customDomains의 FQDN 매치",
			subDomain: "{vars.svc}",
			cache:     staticCache(snap),
			vars:      map[string]string{"vars.svc": "web"},
			want:      true,
		},
		{
			name:      "online 아님",
			subDomain: "{vars.svc}--{vars.ns}",
			cache:     staticCache(snap),
			vars:      map[string]string{"vars.svc": "api", "vars.ns": "feature-y"},
			want:      false,
		},
		{
			name:      "스냅샷 없으면 fail-closed",
			subDomain: "{vars.svc}--{vars.ns}",
			cache:     staticCache(nil),
			vars:      map[string]string{"vars.svc": "api", "vars.ns": "feature-x"},
			want:      false,
		},
		{
			name:      "치환 결과가 비면 매치하지 않음",
			subDomain: "{vars.svc}",
			cache:     staticCache(snap),
			vars:      map[string]string{},
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &MatchFrpOnline{
				SubDomain:     tt.subDomain,
				SubDomainHost: "frp.example.com",
				// Provision을 거치지 않으므로 정규화된 값을 직접 채운다.
				subDomainHost: "frp.example.com",
				cache:         tt.cache,
			}
			got, err := m.MatchWithError(newTestRequest(tt.vars))
			if err != nil {
				t.Fatalf("MatchWithError는 에러를 반환하면 안 된다: %v", err)
			}
			if got != tt.want {
				t.Errorf("매치 결과 = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestProvisionAppliesDefaultsAndSharesCache(t *testing.T) {
	srv, calls := newTestFrps(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"proxies":[{"status":"online","conf":{"subdomain":"api--feature-x"}}]}`)
	})

	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	defer cancel()

	newMatcher := func() *MatchFrpOnline {
		return &MatchFrpOnline{
			AdminAPI:      srv.URL,
			SubDomainHost: "frp.example.com",
			SubDomain:     "{vars.svc}--{vars.ns}",
		}
	}

	first := newMatcher()
	if err := first.Provision(ctx); err != nil {
		t.Fatalf("Provision 실패: %v", err)
	}
	defer first.Cleanup()

	if time.Duration(first.Refresh) != defaultRefresh {
		t.Errorf("refresh 기본값 = %v, want %v", time.Duration(first.Refresh), defaultRefresh)
	}
	if time.Duration(first.Timeout) != defaultTimeout {
		t.Errorf("timeout 기본값 = %v, want %v", time.Duration(first.Timeout), defaultTimeout)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("Provision 초기 조회 횟수 = %d, want 1", got)
	}

	// 설정이 같은 두 번째 matcher는 캐시를 공유하므로 다시 조회하지 않는다.
	second := newMatcher()
	if err := second.Provision(ctx); err != nil {
		t.Fatalf("두 번째 Provision 실패: %v", err)
	}
	defer second.Cleanup()

	if first.cache != second.cache {
		t.Error("설정이 같은데 캐시를 공유하지 않았다")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("캐시 공유 후 조회 횟수 = %d, want 1", got)
	}

	matched, err := first.MatchWithError(newTestRequest(map[string]string{"vars.svc": "api", "vars.ns": "feature-x"}))
	if err != nil {
		t.Fatalf("예상치 못한 에러: %v", err)
	}
	if !matched {
		t.Error("Provision 직후 매치되지 않았다")
	}
}

// TestProvisionDifferentPasswordGetsDifferentCache는 endpoint/user/refresh/timeout이
// 모두 같아도 admin_password만 다르면 캐시를 공유하지 않는지 확인한다.
// 공유하면 비밀번호 회전이 조용히 무시되고 재시작 전까지 반영되지 않는다.
func TestProvisionDifferentPasswordGetsDifferentCache(t *testing.T) {
	srv, _ := newTestFrps(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"proxies":[]}`)
	})

	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	defer cancel()

	newMatcher := func(password string) *MatchFrpOnline {
		return &MatchFrpOnline{
			AdminAPI:      srv.URL,
			AdminUser:     "admin",
			AdminPassword: password,
			SubDomainHost: "frp.example.com",
			SubDomain:     "{vars.svc}--{vars.ns}",
		}
	}

	first := newMatcher("secret-a")
	if err := first.Provision(ctx); err != nil {
		t.Fatalf("Provision 실패: %v", err)
	}
	defer first.Cleanup()

	second := newMatcher("secret-b")
	if err := second.Provision(ctx); err != nil {
		t.Fatalf("두 번째 Provision 실패: %v", err)
	}
	defer second.Cleanup()

	if first.cache == second.cache {
		t.Error("비밀번호가 다른데 캐시를 공유했다")
	}
}

// 음수 duration은 기본값으로 조용히 바뀌지 않고 거부되어야 한다.
func TestProvisionRejectsNegativeDuration(t *testing.T) {
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	defer cancel()

	m := &MatchFrpOnline{
		AdminAPI:      "http://127.0.0.1:1",
		SubDomainHost: "frp.example.com",
		SubDomain:     "api--feature-x",
		Refresh:       caddy.Duration(-time.Second),
	}
	if err := m.Provision(ctx); err == nil {
		_ = m.Cleanup()
		t.Fatal("음수 refresh가 거부되지 않았다")
	}
}

func TestProvisionSurvivesUnreachableAdminAPI(t *testing.T) {
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	defer cancel()

	m := &MatchFrpOnline{
		// 닫힌 포트로 붙어 조회가 실패하도록 만든다.
		AdminAPI:      "http://127.0.0.1:1",
		SubDomainHost: "frp.example.com",
		SubDomain:     "{vars.svc}--{vars.ns}",
		Timeout:       caddy.Duration(100 * time.Millisecond),
	}
	if err := m.Provision(ctx); err != nil {
		t.Fatalf("admin API 조회 실패는 기동을 막으면 안 된다: %v", err)
	}
	defer m.Cleanup()

	matched, err := m.MatchWithError(newTestRequest(map[string]string{"vars.svc": "api", "vars.ns": "feature-x"}))
	if err != nil {
		t.Fatalf("MatchWithError는 에러를 반환하면 안 된다: %v", err)
	}
	if matched {
		t.Error("조회에 실패했는데 매치되었다. fail-closed가 아니다")
	}
}

// TestEndToEnd는 실제 frps/frpc로 라우팅 전환을 검증한다.
// frps/frpc는 FRP_BIN_DIR 또는 PATH에서 찾고, 없으면 skip한다.
func TestEndToEnd(t *testing.T) {
	frpsBin, frpcBin := lookupFrpBin(t, "frps"), lookupFrpBin(t, "frpc")

	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "local")
	}))
	defer local.Close()
	k8s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "k8s")
	}))
	defer k8s.Close()

	dir := t.TempDir()
	bindPort, vhostPort, dashPort := freePort(t), freePort(t), freePort(t)
	const subDomainHost = "frp.test"

	frpsConf := writeFile(t, dir, "frps.toml", fmt.Sprintf(`
bindPort = %d
vhostHTTPPort = %d
subDomainHost = %q
webServer.addr = "127.0.0.1"
webServer.port = %d
webServer.user = "admin"
webServer.password = "secret"
`, bindPort, vhostPort, subDomainHost, dashPort))
	frpcConf := writeFile(t, dir, "frpc.toml", fmt.Sprintf(`
serverAddr = "127.0.0.1"
serverPort = %d
[[proxies]]
name = "api--feature-x"
type = "http"
localPort = %s
subdomain = "api--feature-x"
`, bindPort, local.URL[strings.LastIndex(local.URL, ":")+1:]))

	frps := startProcess(t, frpsBin, frpsConf)
	waitPort(t, dashPort)

	tester := caddytest.NewTester(t)
	tester.InitServer(fmt.Sprintf(`
	{
		skip_install_trust
		admin localhost:2999
		http_port 9080
		https_port 9443
	}
	http://localhost:9080 {
		route {
			vars svc api
			vars ns feature-x
			@frp frp_online {
				admin_api http://127.0.0.1:%d
				admin_user admin
				admin_password secret
				sub_domain_host %s
				sub_domain {vars.svc}--{vars.ns}
				refresh 200ms
				timeout 1s
			}
			handle @frp {
				reverse_proxy 127.0.0.1:%d {
					header_up Host "{vars.svc}--{vars.ns}.%s"
				}
			}
			reverse_proxy %s
		}
	}`, dashPort, subDomainHost, vhostPort, subDomainHost, k8s.URL), "caddyfile")

	expectBody(t, tester, "터널 없음", "k8s")

	frpc := startProcess(t, frpcBin, frpcConf)
	expectBody(t, tester, "터널 연결", "local")

	stopProcess(frpc)
	expectBody(t, tester, "터널 종료(offline)", "k8s")

	frpc = startProcess(t, frpcBin, frpcConf)
	expectBody(t, tester, "터널 재연결", "local")

	stopProcess(frps)
	expectBody(t, tester, "frps 다운(fail-closed)", "k8s")
	stopProcess(frpc)
}

// expectBody는 응답 본문이 want가 될 때까지 최대 10초 폴링한다.
func expectBody(t *testing.T, tester *caddytest.Tester, step, want string) {
	t.Helper()
	var got string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		res, err := tester.Client.Get("http://localhost:9080/")
		if err == nil {
			body, _ := io.ReadAll(res.Body)
			_ = res.Body.Close()
			got = fmt.Sprintf("%d %s", res.StatusCode, body)
			if res.StatusCode == http.StatusOK && string(body) == want {
				t.Logf("%s: %s", step, want)
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s: want %q, last got %q", step, want, got)
}

func startProcess(t *testing.T, bin, conf string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(bin, "-c", conf)
	if err := cmd.Start(); err != nil {
		t.Fatalf("%s 시작 실패: %v", bin, err)
	}
	t.Cleanup(func() { stopProcess(cmd) })
	return cmd
}

func stopProcess(cmd *exec.Cmd) {
	if cmd.ProcessState != nil {
		return
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

func waitPort(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("port %d 대기 시간 초과", port)
}

func lookupFrpBin(t *testing.T, name string) string {
	t.Helper()
	if dir := os.Getenv("FRP_BIN_DIR"); dir != "" {
		return filepath.Join(dir, name)
	}
	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s 바이너리 없음. FRP_BIN_DIR 로 지정한다", name)
	}
	return path
}
