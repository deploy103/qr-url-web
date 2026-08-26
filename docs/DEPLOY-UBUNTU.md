# Ubuntu 배포 가이드 (systemd)

Ubuntu Server 24.04 LTS 기준입니다. Docker로 배포하려면
[`DEPLOY-DOCKER.md`](DEPLOY-DOCKER.md) 를 보세요.

최종 구성:

```
Internet → Nginx (TLS 종료, 443) → 127.0.0.1:8080 (qrurl) → SQLite (WAL)
```

애플리케이션은 **루프백에만 바인드**하며, 외부에서 직접 접근할 수 없습니다.

---

## 0. 사전 준비

* `url.mvtp.cloud` A/AAAA 레코드가 서버를 가리킬 것
* 방화벽에서 22, 80, 443만 열려 있을 것
* sudo 권한이 있는 계정

```bash
sudo apt update && sudo apt upgrade -y
sudo apt install -y nginx certbot python3-certbot-nginx ufw
```

방화벽:

```bash
sudo ufw allow OpenSSH
sudo ufw allow 'Nginx Full'
sudo ufw enable
sudo ufw status
```

---

## 1. 바이너리 준비

### 1-a. 서버에서 직접 빌드

```bash
# Go 1.27 설치
curl -fsSLO https://go.dev/dl/go1.27.0.linux-amd64.tar.gz
sha256sum go1.27.0.linux-amd64.tar.gz   # go.dev/dl 의 값과 대조
sudo rm -rf /usr/local/go
sudo tar -C /usr/local -xzf go1.27.0.linux-amd64.tar.gz
export PATH=$PATH:/usr/local/go/bin
go version

# 소스 빌드
git clone <저장소 주소> qr_url_service
cd qr_url_service
make build            # dist/qrurl, dist/qrdecode
```

### 1-b. 로컬에서 크로스 컴파일 후 업로드

```bash
make build-linux                       # dist/qrurl-linux-amd64
scp dist/qrurl-linux-amd64 server:/tmp/qrurl
```

`CGO_ENABLED=0` 으로 빌드하므로 정적 바이너리 하나만 있으면 됩니다.
SQLite는 순수 Go 구현이 바이너리에 포함되어 있어 `libsqlite3` 설치가 필요 없습니다.

---

## 2. 사용자와 디렉터리

```bash
sudo useradd --system --home-dir /var/lib/qrurl --shell /usr/sbin/nologin qrurl

sudo install -d -o root  -g root  -m 0755 /opt/qrurl /opt/qrurl/bin
sudo install -d -o qrurl -g qrurl -m 0750 /var/lib/qrurl
sudo install -d -o qrurl -g qrurl -m 0750 /var/lib/qrurl/qr
sudo install -d -o qrurl -g qrurl -m 0750 /var/lib/qrurl/backups
sudo install -d -o root  -g qrurl -m 0750 /etc/qrurl

sudo install -o root -g root -m 0755 dist/qrurl /opt/qrurl/bin/qrurl
sudo install -o root -g root -m 0755 dist/qrdecode /opt/qrurl/bin/qrdecode
sudo install -o root -g root -m 0644 README.md /opt/qrurl/README.md
```

---

## 3. 환경 설정

```bash
sudo install -o root -g qrurl -m 0640 deploy/qrurl.env.example /etc/qrurl/qrurl.env
sudo nano /etc/qrurl/qrurl.env
```

최소한 다음이 맞는지 확인하세요.

```ini
QRURL_ADDR=127.0.0.1:8080
QRURL_BASE_URL=https://url.mvtp.cloud
QRURL_DATA_DIR=/var/lib/qrurl
QRURL_SECURE_COOKIE=true
QRURL_HSTS=true
QRURL_TRUST_PROXY=true
QRURL_TRUSTED_PROXIES=127.0.0.1/32,::1/128
QRURL_TIMEZONE=Asia/Seoul
QRURL_REDIRECT_RATE=120
QRURL_SINGLE_INSTANCE=true
```

`QRURL_TRUSTED_PROXIES` 는 `X-Real-IP` / `X-Forwarded-For` 를 믿어도 되는 peer
목록입니다. Nginx가 같은 호스트에 있으므로 기본값 그대로 두면 됩니다. Nginx를
다른 장비에 두었다면 그 장비의 주소를 넣으세요. 목록 밖에서 들어온 요청의
헤더는 무시되므로, 포트에 직접 접근한 공격자가 자기 주소를 위조해 rate limit을
우회할 수 없습니다.

`QRURL_BASE_URL` 은 Short URL QR의 payload에 그대로 들어갑니다. 나중에 바꾸면
이미 발행된 Short URL QR과 값이 어긋나므로, 배포 전에 확정하세요.

---

## 4. systemd

```bash
sudo install -o root -g root -m 0644 deploy/qrurl.service /etc/systemd/system/qrurl.service
sudo systemctl daemon-reload
sudo systemctl enable --now qrurl
sudo systemctl status qrurl
```

첫 실행 로그에서 자동 생성된 관리자 비밀번호를 확인합니다.

```bash
sudo journalctl -u qrurl -n 50 --no-pager
```

바로 원하는 비밀번호로 바꾸는 편이 좋습니다.

```bash
sudo -u qrurl QRURL_DATA_DIR=/var/lib/qrurl /opt/qrurl/bin/qrurl setpassword
```

로컬에서 동작 확인:

```bash
curl -s http://127.0.0.1:8080/health
# {"status":"ok","time":"...","uptimeSeconds":3,"version":"1.0.0","sqlite":"3.53.3","schemaVersion":2}
```

버전과 SQLite 정보는 루프백에서 호출할 때만 포함됩니다. 외부에서 보이는
응답에는 상태와 가동 시간만 들어갑니다.

---

## 5. Nginx

```bash
sudo install -d -m 0755 /etc/nginx/snippets
sudo install -o root -g root -m 0644 deploy/nginx/qrurl-proxy.conf /etc/nginx/snippets/qrurl-proxy.conf
sudo install -o root -g root -m 0644 deploy/nginx/url.mvtp.cloud.conf /etc/nginx/sites-available/url.mvtp.cloud
sudo ln -sf /etc/nginx/sites-available/url.mvtp.cloud /etc/nginx/sites-enabled/url.mvtp.cloud
sudo rm -f /etc/nginx/sites-enabled/default
```

`limit_req_zone` 지시자는 http 컨텍스트에만 올 수 있습니다. 배포한 설정 파일의
맨 위 세 줄을 잘라내 별도 파일로 옮기세요.

```bash
sudo tee /etc/nginx/conf.d/qrurl-limits.conf >/dev/null <<'EOF'
limit_req_zone $binary_remote_addr zone=qrurl_public:10m rate=20r/s;
limit_req_zone $binary_remote_addr zone=qrurl_admin:10m  rate=5r/s;
limit_req_zone $binary_remote_addr zone=qrurl_login:10m  rate=10r/m;
EOF
sudo sed -i '/^limit_req_zone/d' /etc/nginx/sites-available/url.mvtp.cloud
```

### 인증서 발급

`url.mvtp.cloud.conf` 는 인증서가 이미 있다고 가정합니다. 처음이라면 80 포트
블록만 남긴 상태에서 certbot을 돌린 뒤 443 블록을 활성화하세요.

```bash
sudo certbot certonly --nginx -d url.mvtp.cloud --agree-tos -m you@example.com --no-eff-email
sudo nginx -t
sudo systemctl reload nginx
```

자동 갱신 확인:

```bash
sudo systemctl status certbot.timer
sudo certbot renew --dry-run
```

---

## 6. 동작 확인

```bash
# 리다이렉트 대상이 없으므로 404 페이지가 떠야 정상
curl -sI https://url.mvtp.cloud/001 | head -1

# 공개 QR 생성기
curl -sI https://url.mvtp.cloud/ | head -1

# 보안 헤더
curl -sI https://url.mvtp.cloud/ | grep -iE 'strict-transport|content-security|x-frame|x-content-type'

# health 는 외부에서 막혀 있어야 함 (403)
curl -sI https://url.mvtp.cloud/health | head -1

# 관리자 페이지는 로그인으로 리다이렉트 (303)
curl -sI https://url.mvtp.cloud/adminpage | head -1
```

브라우저에서 <https://url.mvtp.cloud/adminpage> 에 로그인한 뒤:

1. 단축 URL 생성 (자동 번호 → `001`)
2. `https://url.mvtp.cloud/001` 접속 → 대상으로 이동, 클릭 수 증가 확인
3. QR 코드 생성 (Short URL QR) → PNG 다운로드
4. 스마트폰 카메라로 스캔 → `https://url.mvtp.cloud/001` 로 이동
5. 만료 1시간으로 설정 → 시간 경과 후 접속하면 410

QR payload 검증은 서버에서도 가능합니다.

```bash
curl -s 'https://url.mvtp.cloud/qr.png?data=https%3A%2F%2Fexample.com%2Fabc%3Fa%3D1%26b%3D2' -o /tmp/t.png
/opt/qrurl/bin/qrdecode /tmp/t.png
# https://example.com/abc?a=1&b=2
```

---

## 7. 운영

### 로그

```bash
sudo journalctl -u qrurl -f
sudo journalctl -u qrurl --since '1 hour ago' | jq -r 'select(.msg=="request")'
sudo tail -f /var/log/nginx/url.mvtp.cloud.access.log
```

### 백업

주기적 백업은 `QRURL_BACKUP_INTERVAL` 로 자동 실행되며 `QRURL_BACKUP_KEEP` 개를
유지합니다. 수동 백업:

```bash
sudo -u qrurl QRURL_DATA_DIR=/var/lib/qrurl /opt/qrurl/bin/qrurl backup
ls -l /var/lib/qrurl/backups
```

원격 보관 예시(서버 밖으로 옮기는 것을 권장):

```bash
sudo tar czf - -C /var/lib/qrurl backups qr | ssh backup-host 'cat > qrurl-$(date +%F).tar.gz'
```

### 복구

```bash
sudo systemctl stop qrurl
sudo -u qrurl cp /var/lib/qrurl/backups/qrurl-20260826-030000.db /var/lib/qrurl/qrurl.db
sudo -u qrurl rm -f /var/lib/qrurl/qrurl.db-wal /var/lib/qrurl/qrurl.db-shm
sudo systemctl start qrurl
```

`qr/` 디렉터리의 이미지 파일이 없어도 서비스는 정상 동작합니다. 관리자 페이지에서
"다시 생성"을 누르면 payload로부터 다시 렌더링됩니다.

### 업그레이드

```bash
make build-linux
scp dist/qrurl-linux-amd64 server:/tmp/qrurl

sudo -u qrurl QRURL_DATA_DIR=/var/lib/qrurl /opt/qrurl/bin/qrurl backup
sudo systemctl stop qrurl
sudo install -o root -g root -m 0755 /tmp/qrurl /opt/qrurl/bin/qrurl
sudo systemctl start qrurl
sudo systemctl status qrurl
curl -s http://127.0.0.1:8080/health
```

스키마 마이그레이션은 시작할 때 자동으로 적용됩니다.

### 비밀번호 변경

관리자 페이지의 **설정** 탭에서 현재 비밀번호를 입력해 변경할 수 있습니다.
이 경우 지금 쓰고 있는 브라우저의 세션은 새 토큰으로 회전되고 나머지 세션만
로그아웃됩니다.

비밀번호를 잊었을 때는 서버에서 재설정합니다.

```bash
sudo -u qrurl QRURL_DATA_DIR=/var/lib/qrurl /opt/qrurl/bin/qrurl setpassword
```

CLI로 바꾸면 기존 세션이 전부 무효화됩니다.

### 감사 로그

로그인 성공·실패, 생성·수정·삭제, 만료 변경, 정리 실행, 내보내기가 소스 IP와
함께 180일간 보관됩니다. 설정 탭에서 확인하거나 내보낼 수 있습니다.

```bash
# 최근 로그인 실패만 빠르게 확인
sudo -u qrurl sqlite3 /var/lib/qrurl/qrurl.db \
  "SELECT datetime(created_at,'unixepoch','localtime'), ip, detail
     FROM audit_log WHERE action='login.failed' ORDER BY id DESC LIMIT 20;"
```

`sqlite3` CLI가 없다면 설정 탭의 **감사 로그 CSV** 버튼을 사용하세요.

### 목록 내보내기

설정 탭에서 단축 URL·QR 목록·감사 로그를 CSV 또는 JSON으로 내려받을 수
있습니다. 이는 조회용 사본이며, 복구 가능한 백업은 `qrurl backup` 입니다.

---

## 8. 문제 해결

| 증상 | 확인할 것 |
|---|---|
| 502 Bad Gateway | `systemctl status qrurl`, `ss -tlnp \| grep 8080` |
| 로그인 후 다시 로그인 화면 | `QRURL_SECURE_COOKIE=true` 인데 HTTP로 접속했는지 확인 |
| CSRF 오류가 계속 발생 | Nginx가 `Host` 헤더를 그대로 전달하는지, `QRURL_BASE_URL` 이 실제 도메인과 같은지 |
| QR 미리보기가 414/400 | Nginx `large_client_header_buffers 4 32k;` 가 적용됐는지 |
| 만료 항목이 남아 있음 | 관리자 페이지의 "만료 정리 실행" 버튼, 그리고 `QRURL_CLEANUP_INTERVAL` |
| `database is locked` | `/var/lib/qrurl` 을 두 인스턴스가 공유하고 있지 않은지 |
| 파일 권한 오류 | `/var/lib/qrurl` 소유자가 `qrurl:qrurl`, 모드 `0750` 인지 |
| rate limit이 모든 사용자에게 걸림 | `QRURL_TRUSTED_PROXIES` 가 Nginx 주소를 포함하는지. 빠져 있으면 모든 요청이 프록시 IP 하나로 집계됨 |
| 감사 로그에 IP가 전부 127.0.0.1 | 위와 동일. Nginx가 `X-Real-IP` 를 설정하는지도 확인 |

systemd 샌드박싱 때문에 쓰기가 막힌다면 유닛의 `ReadWritePaths=/var/lib/qrurl` 이
실제 데이터 디렉터리와 일치하는지 확인하세요.

---

## 9. 제거

```bash
sudo systemctl disable --now qrurl
sudo rm -f /etc/systemd/system/qrurl.service
sudo systemctl daemon-reload
sudo rm -rf /opt/qrurl /etc/qrurl
sudo rm -f /etc/nginx/sites-enabled/url.mvtp.cloud /etc/nginx/sites-available/url.mvtp.cloud
sudo rm -f /etc/nginx/snippets/qrurl-proxy.conf /etc/nginx/conf.d/qrurl-limits.conf
sudo systemctl reload nginx
# 데이터까지 지울 때만:
# sudo rm -rf /var/lib/qrurl && sudo userdel qrurl
```
