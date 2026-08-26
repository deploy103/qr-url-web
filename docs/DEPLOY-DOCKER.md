# Docker 배포 가이드 (mvtp-web)

기존 `mvtp-web` 구성에 맞춘 배포 절차입니다. 다른 서비스와 동일하게 컨테이너를
`mvtp-web-net` 에 올리고, 호스트에는 포트를 공개하지 않으며, `internal-nginx` 가
컨테이너 이름으로 라우팅합니다.

```
Internet → edge → internal-nginx (192.0.2.50:80) → qr-url:8013 → SQLite (WAL, volume)
```

---

## 0. 사전 확인

```bash
docker network ls | grep mvtp-web-net
docker ps --format '{{.Names}}\t{{.Ports}}' | grep internal-nginx
```

`mvtp-web-net` 이 이미 있고 `internal-nginx` 가 거기 붙어 있으면 그대로 진행합니다.

---

## 1. 소스 배치와 환경 파일

다른 서비스와 같은 위치 규칙을 따릅니다.

```bash
cd ~/services            # 기존 compose 디렉터리들이 있는 곳
git clone <저장소 주소> qr-url
cd qr-url
```

첫 기동에만 쓸 `.env` 를 만듭니다.

```bash
cat > .env <<'EOF'
QRURL_VERSION=1.2.0
QRURL_BASE_URL=https://url.mvtp.cloud
QRURL_TIMEZONE=Asia/Seoul

# internal-nginx 가 있는 Docker 네트워크만 신뢰합니다.
QRURL_TRUSTED_PROXIES=172.16.0.0/12

# 공개 QR 생성기를 사내망으로만 열려면 아래 주석을 해제하세요.
#QRURL_PUBLIC_QR_ALLOW=10.0.0.0/24

# 최초 1회만. 기동 후 반드시 지우고 `docker compose up -d` 로 다시 올리세요.
QRURL_ADMIN_USERNAME=admin
QRURL_ADMIN_PASSWORD=여기에-충분히-긴-비밀번호
EOF
chmod 600 .env
```

`.env` 를 쓰지 않으면 첫 기동 시 무작위 비밀번호가 로그에 한 번 출력됩니다.

---

## 2. 빌드와 기동

```bash
# 이미지 안에서 go vet + 전체 테스트를 먼저 돌려봅니다(선택).
docker compose build
docker build --target test .

docker compose up -d
docker compose ps
docker compose logs -f qr-url
```

기동 로그에서 확인할 것:

```
msg=starting addr=0.0.0.0:8013 baseURL=https://url.mvtp.cloud sqlite=3.53.3
  trustedProxies=172.16.0.0/12 redirectRate=120 secureCookie=true hsts=true
```

컨테이너 안에서 상태 확인:

```bash
docker compose exec qr-url wget -qO- http://127.0.0.1:8013/health
# {"status":"ok",...,"version":"1.2.0","sqlite":"3.53.3","schemaVersion":2}
```

비밀번호를 `.env` 로 넣었다면 지금 지웁니다.

```bash
sed -i '/^QRURL_ADMIN_PASSWORD=/d;/^QRURL_ADMIN_USERNAME=/d' .env
docker compose up -d
```

---

## 3. internal-nginx 라우팅

vhost와 proxy 스니펫을 `internal-nginx` 안에 넣습니다. 경로는 기존 서비스들이
쓰는 규칙에 맞추세요.

```bash
docker cp deploy/nginx/qrurl-proxy-docker.conf \
  internal-nginx:/etc/nginx/snippets/qrurl-proxy-docker.conf
docker cp deploy/nginx/internal-url.mvtp.cloud.conf \
  internal-nginx:/etc/nginx/conf.d/url.mvtp.cloud.conf

docker exec internal-nginx nginx -t
docker exec internal-nginx nginx -s reload
```

호스트 볼륨으로 nginx 설정을 관리한다면 `docker cp` 대신 그 디렉터리에 복사한 뒤
reload 하세요. 그편이 컨테이너 재생성 후에도 유지됩니다.

DNS 이름이 잡히는지 확인:

```bash
docker exec internal-nginx getent hosts qr-url
docker exec internal-nginx wget -qO- --header 'Host: url.mvtp.cloud' http://qr-url:8013/health
```

`mvtp-check.sh` 의 `[9] PROXY DOCKER DNS` 와 `[10] VHOST HTTP` 항목에
`url.mvtp.cloud` 를 추가하면 이후 점검에 함께 잡힙니다.

---

## 4. 동작 확인

```bash
curl -sI -H 'Host: url.mvtp.cloud' http://192.0.2.50/            # 200
curl -sI -H 'Host: url.mvtp.cloud' http://192.0.2.50/adminpage   # 303 → /adminpage/login
curl -sI -H 'Host: url.mvtp.cloud' http://192.0.2.50/001         # 404 (아직 없음)

# 보안 헤더
curl -sI -H 'Host: url.mvtp.cloud' http://192.0.2.50/ | \
  grep -iE 'content-security|x-frame|x-robots|referrer'
```

브라우저에서 `https://url.mvtp.cloud/adminpage` 로 로그인한 뒤 단축 URL과
Short URL QR을 만들고, 스마트폰 카메라로 스캔해 실제로 이동하는지 확인합니다.

서버에서 payload를 검증할 수도 있습니다.

```bash
curl -s -H 'Host: url.mvtp.cloud' \
  'http://192.0.2.50/qr.png?data=https%3A%2F%2Fexample.com%2Fabc%3Fa%3D1%26b%3D2' -o /tmp/t.png
docker compose exec -T qr-url qrdecode /dev/stdin < /tmp/t.png 2>/dev/null || \
  docker compose cp /tmp/t.png qr-url:/tmp/t.png && docker compose exec qr-url qrdecode /tmp/t.png
# https://example.com/abc?a=1&b=2
```

---

## 5. 운영

### 로그

```bash
docker compose logs -f qr-url
docker compose logs qr-url --since 1h | jq -r 'select(.msg=="request")'
```

로그는 `json-file` 드라이버로 10 MB × 5개까지만 보관하도록 제한되어 있습니다.

### 백업

컨테이너 안에서 주기 백업이 돌고(`QRURL_BACKUP_INTERVAL`), 볼륨 안에 쌓입니다.
수동 백업과 반출:

```bash
docker compose exec qr-url qrurl backup
docker compose exec qr-url ls -l /data/backups

# 볼륨 전체를 호스트로 빼내기
docker run --rm -v qr-url-data:/data -v "$PWD:/out" alpine \
  tar czf /out/qr-url-$(date +%F).tar.gz -C /data .
```

### 복구

```bash
docker compose stop qr-url
docker run --rm -v qr-url-data:/data alpine sh -c \
  'cp /data/backups/qrurl-20260826-030000.db /data/qrurl.db && rm -f /data/qrurl.db-wal /data/qrurl.db-shm'
docker compose start qr-url
```

`/data/qr` 의 이미지가 없어도 서비스는 동작하고, 관리자 페이지의 "다시 생성"
으로 payload에서 복원됩니다.

### 업그레이드

```bash
git pull
docker compose exec qr-url qrurl backup
docker compose up -d --build
docker compose ps
```

스키마 마이그레이션은 기동 시 자동 적용됩니다.

### 비밀번호 재설정

웹의 **설정** 탭에서 바꾸는 것이 기본입니다. 잊었다면 컨테이너에서:

```bash
docker compose exec qr-url qrurl setpassword -stdin <<< '새-비밀번호'
```

### 감사 로그

```bash
docker compose exec qr-url sh -c \
  'wget -qO- --header "Cookie: $C" http://127.0.0.1:8013/adminpage/api/account' >/dev/null
```

실무에서는 설정 탭의 **감사 로그 CSV** 버튼을 쓰는 편이 빠릅니다.

---

## 6. 문제 해결

| 증상 | 확인할 것 |
|---|---|
| 502 / 배드 게이트웨이 | `docker compose ps`, `docker exec internal-nginx getent hosts qr-url` |
| 컨테이너가 즉시 종료 | `docker compose logs qr-url` — 다른 인스턴스가 볼륨을 잡고 있으면 명시적으로 거부합니다 |
| `another instance is already using this data directory` | 같은 볼륨을 쓰는 컨테이너가 남아 있는지 `docker ps -a` 로 확인 |
| 로그인 후 다시 로그인 화면 | `QRURL_SECURE_COOKIE=true` 인데 HTTP로 접근했는지 |
| 감사 로그 IP가 전부 게이트웨이 주소 | `QRURL_TRUSTED_PROXIES` 가 Docker 대역을 포함하는지, nginx가 `X-Real-IP` 를 넘기는지 |
| QR 미리보기가 414/400 | internal-nginx 에 `large_client_header_buffers 4 32k;` 가 적용됐는지 |
| 리다이렉트가 429 | `QRURL_REDIRECT_RATE` 상한. 신뢰 프록시 설정이 빠지면 모든 요청이 프록시 IP 하나로 집계되어 쉽게 걸립니다 |

---

## 7. 제거

```bash
docker compose down
docker exec internal-nginx rm -f /etc/nginx/conf.d/url.mvtp.cloud.conf \
  /etc/nginx/snippets/qrurl-proxy-docker.conf
docker exec internal-nginx nginx -s reload
# 데이터까지 지울 때만:
# docker volume rm qr-url-data
```
