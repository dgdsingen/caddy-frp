# caddy-frps

## 목표

Caddy가 k8s Service로 보내던 트래픽을, 개발자가 로컬 서버를 frp 터널로 노출한 동안에만 그 로컬 서버로 보낸다.
기존 `dynamic multi` / SRV upstream 구문은 건드리지 않는다.

이를 위해 request matcher `frp_online` (`http.matchers.frp_online`) 을 제공한다.
터널이 online이면 매치되어 `handle @frp` 로 분기하고, 아니면 기존 `reverse_proxy` 로 흘러간다.

## 기준 버전

- Go 1.26.7, Caddy v2.11.9, frp v0.72.0 (v0.71.0 에서도 E2E 통과)
- module: `github.com/dgdsingen/caddy-frps`

## 파일

```
frp.go        모듈 전체 (Caddyfile 파싱, 검증, frps 클라이언트, 캐시, matcher)
frp_test.go   단위 테스트 + 실제 frps/frpc 를 띄우는 E2E
Caddyfile     예제 설정
```

## 동작

- frps dashboard `GET /api/proxy/http` (basic auth) 를 폴링해 `status == "online"` 인 프록시의
  `conf.subdomain` / `conf.customDomains` 를 소문자 집합으로 보관한다
- 실제 frps 응답에서 offline 프록시는 `conf: null` 로 목록에 남는다. frps vhost 는 Host 를 소문자로 비교한다
- 매치 조건: `sub_domain` 치환값이 subdomain 집합에 있거나, `{sub_domain}.{sub_domain_host}` 가 customDomains 에 있다
- 요청 경로는 map 조회만 한다. `refresh` 가 지나면 stale 스냅샷으로 응답하면서 goroutine 하나가 갱신한다
- 조회 실패 시 스냅샷을 버린다 (fail-closed). 판정 불가는 항상 매치 실패, 즉 기존 경로
- `MatchWithError` 는 에러를 반환하지 않는다. 에러를 내면 Caddy 가 error 핸들러로 보내 폴백이 깨진다
- 캐시는 `caddy.UsagePool` 로 config reload 를 넘어 유지되고, 접속 설정이 같은 matcher 끼리 공유한다.
  키에는 비밀번호 대신 SHA-256 digest 를 넣어 비밀번호 회전 시 새 캐시를 만든다

## 설정

```
옵션              필수  기본  설명
admin_api         O     -     frps dashboard 주소. http/https, userinfo 금지
sub_domain_host   O     -     frps subDomainHost 값과 같아야 한다
sub_domain        O     -     placeholder 사용 가능
admin_user        X     -     admin_password 와 함께 설정
admin_password    X     -     admin_user 와 함께 설정
refresh           X     10s   양수
timeout           X     3s    양수
```

운영 Caddyfile 에서는 `route` 안, `vars.svc` / `vars.ns` 확정 뒤, 기존 `reverse_proxy` 앞에 둔다.

```caddy
@frp frp_online {
	admin_api "{$FRP_ADMIN_API}"
	admin_user "{$FRP_ADMIN_USER}"
	admin_password "{$FRP_ADMIN_PASSWORD}"
	sub_domain_host "{$FRP_SUB_DOMAIN_HOST}"
	sub_domain {vars.svc}--{vars.ns}
}
handle @frp {
	reverse_proxy "{$FRP_VHOST}" {
		header_up Host "{vars.svc}--{vars.ns}.{$FRP_SUB_DOMAIN_HOST}"
	}
}
```

- `header_up Host` 필수. frps vhost 가 Host 로 프록시를 찾는다
- `{$VAR}` 는 반드시 따옴표로 감싼다. 값이 비면 토큰이 사라져 파싱이 실패한다
- `admin off` 유지. 켜면 `GET /config/` 에 `admin_password` 가 평문으로 나온다

## 검증

```sh
go vet ./... && gofmt -l .
go test ./... -count=1 -race
FRP_BIN_DIR=/path/to/frp-0.72.0 go test ./... -count=1 -run TestEndToEnd -v
xcaddy build v2.11.9 --with github.com/dgdsingen/caddy-frps=.
```

- `TestEndToEnd` 는 frps/frpc 를 `FRP_BIN_DIR` 또는 PATH 에서 찾고, 없으면 skip 한다
- 시나리오: 터널 없음 > k8s, 연결 > local, frpc 종료 > k8s, 재연결 > local, frps 종료 > k8s
- 2026-11-21 frp 0.72.0 (소스 빌드) / 0.71.0 둘 다 통과. fail-closed 를 일부러 깨면 frps 종료 단계에서 502 로 실패함을 확인

## 하지 말 것

- `max_stale` 같은 유예 보관. frps 가 죽으면 터널도 끊기므로 죽은 터널로 502 만 늘어난다
- dynamic upstream 모듈 / placeholder 모듈 방식. Host 재작성 불가, fail-closed 표현이 흐려진다
- deprecated `Match(*http.Request) bool` 구현. Caddy 는 `RequestMatcherWithError` 만 호출한다
- v2 API (`/api/v2/proxies`). 페이지네이션이라 전체 목록에 N 회 호출이 필요하다
- 서브에이전트 보고를 검증 없이 믿기. `git log` 와 테스트 실행으로 확인한다

## 남은 일

- push: `git push -u origin main` (사용자가 직접. `origin/main` 이 gone 상태라 새로 만든다)
- frps 구성: `subDomainHost` 를 `sub_domain_host` 와 맞추고, dashboard 포트(기본 7500)를 Caddy 에서 접근 가능하게 연다
- 운영 Caddyfile 에 위 `@frp` 블록 삽입

## 알려진 한계

- customDomains 는 정확 일치만 본다. 와일드카드 (`*.example.com`) 는 매치하지 않는다
- frpc 가 끊긴 뒤 최대 `refresh` 동안 죽은 터널로 보낼 수 있다 (502)
- 백그라운드 갱신 logger 는 캐시 생성 시점의 것을 계속 쓴다. 로그 출력을 파일로 바꾸고 reload 하면 Warn 이 유실될 수 있다
