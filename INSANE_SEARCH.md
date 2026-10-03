# insane-search 엔진 + AI 쌀먹 스캐너 운영 노트

## 엔진 바이너리
- `bin/insane` — `src/insane-search-migration/cmd/insane` 에서 빌드 (Go 1.27.1, `CGO_ENABLED=0`, `-mod=mod`).
- 주의: `vendor/`는 Go 모듈 vendoring이 아니라 원본 스냅샷이므로 `-mod=mod`로 빌드해야 함.

## 샌드박스 DNS 패치 (내 환경 전용)
- 문제: 이 VM의 DNS는 모든 호스트를 198.18.x.x 싱크홀로 돌려보냄. 엔진의 URLGuard는 fail-closed라
  `ssrf_blocked:resolves_internal` 로 모든 fetch가 실패했음. `/etc/hosts`, `/etc/resolv.conf`는 read-only라 수정 불가.
- 해결: `internal/engine/doh_resolver.go` 추가 + `transport.go`의 `NewTransport()`에서 opt-in 연결.
  `INSANE_DOH_URL` 환경변수가 설정되면 guard의 resolver를 DNS-over-HTTPS(프록시 경유)로 교체.
- 보안 속성 유지: DNS는 라우팅에만 사용, TLS 인증서 검증(SNI=원본 호스트명)이 피어를 인증하므로
  오염된 DNS 응답은 핸드셰이크에서 fail-closed. SSRF BlockedIP 필터도 그대로 적용.
- 환경변수 미설정 시 기존 동작 그대로 (fail-closed).
- 필수 env: `INSANE_DOH_URL=https://cloudflare-dns.com/dns-query` + 인증정보 없는 프록시
  (`HTTPS_PROXY=http://hatch-egress-proxy:3128` — userinfo가 있으면 엔진이 거부함).

## 스캔 동작 확인 (2026-10-03)
- `insane scan` dry-run: dcinside(85), linux.do(150), v2ex(52) 파싱 성공, 후보 28건.
- nodeloc 2개 소스는 로그인 월(`login_required`)이라 수집 불가 — 엔진이 정상 판단한 것이며 사이트 측 제한.

## 크론잡: ai-promo-scanner
- 4시간 간격 interval. 원본 imported-job(240분)의 이식판.
- 순서: `insane scan` → `imported_job.prompt` 품질 계약으로 심사 → 채택 1건 이상이면 `bin/send-dm.sh`로 DM, 0건이면 침묵.
- 품질 계약 원본: `src/insane-search-migration/config/imported-job.disabled.json`의 `imported_job.prompt`.
- 상태: `state/insane/state.json` (신규/변경분 추적용).

## 재빌드
```bash
export PATH=$HOME/.local/go/bin:$PATH
cd ~/workspace/discord-gateway/src/insane-search-migration
CGO_ENABLED=0 go build -mod=mod -trimpath -o ~/workspace/discord-gateway/bin/insane ./cmd/insane
```
