/* Administrator console.
   Every value that comes from the API is inserted with textContent or as an
   attribute of an element built by createElement, so no server data is ever
   parsed as HTML. Mutating requests carry the session bound CSRF token. */
(function () {
  'use strict';

  const main = document.querySelector('.page-admin');
  if (!main) return;

  // The token is rotated when the password changes, so it lives in a variable
  // rather than a constant.
  let CSRF = main.dataset.csrf || '';
  const BASE_URL = main.dataset.baseUrl || '';

  const state = {
    timezone: undefined,
    expiringWindowHours: 48,
    links: [],
    qrs: [],
    qrTotal: 0,
    qrOffset: 0,
    serverTime: new Date(),
  };

  /* ------------------------------------------------------------ helpers */

  function el(tag, props, children) {
    const node = document.createElement(tag);
    if (props) {
      for (const [key, value] of Object.entries(props)) {
        if (value === undefined || value === null || value === false) continue;
        if (key === 'class') node.className = value;
        else if (key === 'text') node.textContent = value;
        else if (key === 'dataset') Object.assign(node.dataset, value);
        else if (key.startsWith('on') && typeof value === 'function') {
          node.addEventListener(key.slice(2).toLowerCase(), value);
        } else if (value === true) node.setAttribute(key, '');
        else node.setAttribute(key, String(value));
      }
    }
    for (const child of [].concat(children || [])) {
      if (child === null || child === undefined || child === false) continue;
      node.appendChild(typeof child === 'string' ? document.createTextNode(child) : child);
    }
    return node;
  }

  function clear(node) { while (node.firstChild) node.removeChild(node.firstChild); }

  const numberFormat = new Intl.NumberFormat('ko-KR');
  function num(n) { return numberFormat.format(n || 0); }

  function dateFormatter() {
    const opts = {
      year: 'numeric', month: '2-digit', day: '2-digit',
      hour: '2-digit', minute: '2-digit', hour12: false,
    };
    if (state.timezone) opts.timeZone = state.timezone;
    return new Intl.DateTimeFormat('ko-KR', opts);
  }

  function fmtDate(value) {
    if (!value) return '없음';
    try { return dateFormatter().format(new Date(value)); } catch (e) { return String(value); }
  }

  function fmtRemaining(seconds) {
    if (seconds === null || seconds === undefined) return '무제한';
    if (seconds <= 0) return '만료됨';
    const d = Math.floor(seconds / 86400);
    const h = Math.floor((seconds % 86400) / 3600);
    const m = Math.floor((seconds % 3600) / 60);
    if (d > 0) return `${d}일 ${h}시간`;
    if (h > 0) return `${h}시간 ${m}분`;
    if (m > 0) return `${m}분`;
    return `${seconds}초`;
  }

  const STATUS_LABEL = {
    active: '활성', disabled: '비활성', expiring: '곧 만료', expired: '만료됨',
  };
  const KIND_LABEL = {
    short: 'Short URL QR', url: 'Direct URL QR', text: 'Text QR',
  };

  function statusPill(status) {
    return el('span', { class: `pill pill-${status}`, text: STATUS_LABEL[status] || status });
  }

  let toastTimer = null;
  function toast(message) {
    const node = document.getElementById('toast');
    node.textContent = message;
    node.hidden = false;
    window.clearTimeout(toastTimer);
    toastTimer = window.setTimeout(() => { node.hidden = true; }, 2600);
  }

  function showGlobalError(message) {
    const node = document.getElementById('global-error');
    if (!message) { node.hidden = true; return; }
    node.textContent = message;
    node.hidden = false;
  }

  async function api(method, url, body) {
    const options = {
      method,
      headers: { 'Accept': 'application/json' },
      credentials: 'same-origin',
    };
    if (body !== undefined) {
      options.headers['Content-Type'] = 'application/json';
      options.headers['X-CSRF-Token'] = CSRF;
      options.body = JSON.stringify(body);
    } else if (method !== 'GET') {
      options.headers['X-CSRF-Token'] = CSRF;
    }
    const res = await fetch(url, options);
    if (res.status === 401) {
      window.location.href = '/adminpage/login?expired=1';
      throw new Error('세션이 만료되었습니다.');
    }
    let payload = null;
    try { payload = await res.json(); } catch (e) { payload = null; }
    if (!res.ok) {
      throw new Error((payload && payload.error) || `요청이 실패했습니다 (${res.status})`);
    }
    return payload;
  }

  async function copyText(value, label) {
    try {
      await navigator.clipboard.writeText(value);
      toast(`${label} 복사됨`);
    } catch (e) {
      toast('복사에 실패했습니다');
    }
  }

  /* --------------------------------------------------------------- tabs */

  const tabs = Array.from(document.querySelectorAll('.tab'));
  const panels = {
    dashboard: document.getElementById('panel-dashboard'),
    links: document.getElementById('panel-links'),
    qrs: document.getElementById('panel-qrs'),
    settings: document.getElementById('panel-settings'),
  };

  function activate(name) {
    tabs.forEach((t) => {
      const on = t.dataset.tab === name;
      t.classList.toggle('is-active', on);
      t.setAttribute('aria-selected', on ? 'true' : 'false');
    });
    Object.entries(panels).forEach(([key, panel]) => panel.classList.toggle('is-active', key === name));
    if (window.location.hash.slice(1) !== name) {
      history.replaceState(null, '', `#${name}`);
    }
    if (name === 'dashboard') loadOverview();
    if (name === 'links') loadLinks();
    if (name === 'qrs') loadQRs(false);
    if (name === 'settings') loadAccount();
  }

  tabs.forEach((t) => t.addEventListener('click', () => activate(t.dataset.tab)));

  /* ---------------------------------------------------------- dashboard */

  function statCard(label, value, hint) {
    return el('div', { class: 'stat' }, [
      el('span', { class: 'stat-label', text: label }),
      el('span', { class: 'stat-value', text: value }),
      hint ? el('span', { class: 'stat-hint', text: hint }) : null,
    ]);
  }

  function miniRow(title, sub, status) {
    return el('div', { class: 'mini-row' }, [
      el('div', { class: 'mini-main' }, [
        el('span', { class: 'mini-title', text: title }),
        el('span', { class: 'mini-sub', text: sub }),
      ]),
      status ? statusPill(status) : null,
    ]);
  }

  function renderActivity(history) {
    const box = document.getElementById('activity-chart');
    clear(box);
    const days = (history || []).slice().reverse();
    if (!days.length) {
      box.appendChild(el('p', { class: 'empty', text: '아직 기록이 없습니다.' }));
      return;
    }
    const peak = Math.max(1, ...days.map((d) => d.redirects + d.urlsCreated + d.qrsCreated));
    for (const day of days) {
      const total = day.redirects + day.urlsCreated + day.qrsCreated;
      const bar = el('div', {
        class: total > 0 ? 'activity-bar has-value' : 'activity-bar',
        title: `${day.day} · 리다이렉트 ${day.redirects} · URL ${day.urlsCreated} · QR ${day.qrsCreated}`,
      });
      bar.style.height = `${Math.max(3, Math.round((total / peak) * 80))}px`;
      box.appendChild(el('div', { class: 'activity-day' }, [
        bar,
        el('span', { class: 'activity-label', text: day.day.slice(5) }),
      ]));
    }
  }

  async function loadOverview() {
    try {
      const data = await api('GET', '/adminpage/api/overview');
      showGlobalError(null);
      state.timezone = data.timezone;
      state.expiringWindowHours = data.expiringWindowHours;
      state.serverTime = new Date(data.serverTime);

      const d = data.dashboard;
      const grid = document.getElementById('stat-grid');
      clear(grid);
      grid.append(
        statCard('활성 단축 URL', num(d.activeShortURLs), `전체 ${num(d.totalShortURLs)}개 · 비활성 ${num(d.disabledShortURLs)}개`),
        statCard('QR 코드', num(d.totalQRCodes), `만료로 접속 차단 가능 ${num(d.revocableQRCodes)}개`),
        statCard('오늘 생성', num(d.today.urlsCreated + d.today.qrsCreated), `URL ${num(d.today.urlsCreated)} · QR ${num(d.today.qrsCreated)}`),
        statCard('오늘 리다이렉트', num(d.today.redirects), `누적 클릭 ${num(d.totalClicks)}회`),
        statCard('남은 코드', num(d.freeCodes), '001 ~ 999 중 사용 가능'),
      );

      document.getElementById('expiring-window').textContent = `${data.expiringWindowHours}시간 이내`;
      const expiring = document.getElementById('expiring-list');
      clear(expiring);
      const expiringItems = [];
      for (const link of data.expiringShortUrls) {
        expiringItems.push(miniRow(`/${link.code} → ${link.targetUrl}`,
          `단축 URL · ${fmtDate(link.expiresAt)} · 남은 시간 ${fmtRemaining(link.remainingSeconds)}`, link.status));
      }
      for (const qr of data.expiringQrCodes) {
        expiringItems.push(miniRow(qr.label || qr.payload,
          `${KIND_LABEL[qr.kind] || qr.kind} · ${fmtDate(qr.expiresAt)} · 남은 시간 ${fmtRemaining(qr.remainingSeconds)}`, qr.status));
      }
      if (!expiringItems.length) {
        expiring.appendChild(el('p', { class: 'empty', text: '곧 만료되는 항목이 없습니다.' }));
      } else {
        expiringItems.forEach((row) => expiring.appendChild(row));
      }

      const recent = document.getElementById('recent-list');
      clear(recent);
      const recentItems = []
        .concat(data.recentShortUrls.map((l) => ({
          when: l.createdAt,
          node: miniRow(`/${l.code} → ${l.targetUrl}`, `단축 URL · ${fmtDate(l.createdAt)}`, l.status),
        })))
        .concat(data.recentQrCodes.map((q) => ({
          when: q.createdAt,
          node: miniRow(q.label || q.payload, `${KIND_LABEL[q.kind] || q.kind} · ${fmtDate(q.createdAt)}`, q.status),
        })))
        .sort((a, b) => new Date(b.when) - new Date(a.when))
        .slice(0, 10);
      if (!recentItems.length) {
        recent.appendChild(el('p', { class: 'empty', text: '아직 생성한 항목이 없습니다.' }));
      } else {
        recentItems.forEach((item) => recent.appendChild(item.node));
      }

      document.getElementById('activity-tz').textContent = `기준 시간대 ${data.timezone}`;
      // The datetime-local picker shows no timezone, so say which one the
      // server will use when it parses the value.
      document.querySelectorAll('.tz-hint').forEach((node) => {
        node.textContent = `${data.timezone} 기준으로 해석됩니다.`;
      });
      renderActivity(d.history);

      const c = data.cleanup;
      document.getElementById('cleanup-status').textContent =
        `자동 정리 ${num(c.runs)}회 실행 · 마지막 ${c.lastRunAt ? fmtDate(c.lastRunAt) : '없음'} · ` +
        `누적 삭제 URL ${num(c.removedURLs)}개 / QR ${num(c.removedQRs)}개`;
    } catch (e) {
      showGlobalError(e.message);
    }
  }

  /* --------------------------------------------------------- short URLs */

  const linkSearch = document.getElementById('link-search');
  const linkStatus = document.getElementById('link-status');
  const linkSort = document.getElementById('link-sort');
  const linkOrder = document.getElementById('link-order');

  async function loadLinks() {
    const params = new URLSearchParams({
      q: linkSearch.value.trim(),
      status: linkStatus.value,
      sort: linkSort.value,
      order: linkOrder.dataset.order,
    });
    try {
      const data = await api('GET', `/adminpage/api/shorturls?${params.toString()}`);
      showGlobalError(null);
      state.links = data.items || [];
      const summary = document.getElementById('link-summary');
      summary.textContent = data.truncated
        ? `${num(state.links.length)}개 표시 · 전체 ${num(data.total)}개 (일부만 표시됨)`
        : `${num(state.links.length)}개 · 999개 중 ${num(999 - data.total)}개 번호 사용 가능`;
      renderLinks();
    } catch (e) {
      showGlobalError(e.message);
    }
  }

  function metaItem(label, value) {
    return el('span', {}, [el('b', { text: `${label} ` }), value]);
  }

  function renderLinks() {
    const table = document.getElementById('link-table');
    clear(table);
    if (!state.links.length) {
      table.appendChild(el('p', { class: 'empty', text: '조건에 맞는 단축 URL이 없습니다.' }));
      return;
    }
    for (const link of state.links) {
      const badges = el('div', { class: 'row-badges' }, [
        statusPill(link.status),
        link.qrCount > 0 ? el('span', { class: 'pill pill-accent', text: `QR ${link.qrCount}개` }) : null,
      ]);

      const actions = el('div', { class: 'row-actions' }, [
        el('button', {
          class: 'btn btn-sm', type: 'button', text: '복사',
          onclick: () => copyText(link.shortUrl, '단축 URL'),
        }),
        el('button', {
          class: 'btn btn-sm', type: 'button', text: '수정',
          onclick: () => openLinkDialog(link),
        }),
        el('button', {
          class: 'btn btn-sm', type: 'button', text: '만료',
          onclick: () => openExpiryDialog('link', link),
        }),
        el('button', {
          class: 'btn btn-sm', type: 'button', text: 'QR 생성',
          onclick: () => createQRForLink(link),
        }),
        el('button', {
          class: 'btn btn-sm', type: 'button', text: link.enabled ? '비활성화' : '활성화',
          onclick: () => toggleLink(link),
        }),
        el('button', {
          class: 'btn btn-sm btn-danger', type: 'button', text: '삭제',
          onclick: () => deleteLink(link),
        }),
      ]);

      table.appendChild(el('article', { class: 'row-card', dataset: { status: link.status } }, [
        el('span', { class: 'row-code', text: link.code }),
        el('div', { class: 'row-main' }, [
          el('a', { class: 'row-target', href: link.targetUrl, rel: 'noreferrer noopener', target: '_blank', text: link.targetUrl }),
          link.note ? el('span', { class: 'row-note', text: link.note }) : null,
          el('div', { class: 'row-meta' }, [
            metaItem('클릭', num(link.clickCount)),
            metaItem('생성', fmtDate(link.createdAt)),
            metaItem('만료', link.expiresAt ? fmtDate(link.expiresAt) : '없음'),
            metaItem('남은 시간', fmtRemaining(link.remainingSeconds)),
            metaItem('마지막 접속', link.lastAccessedAt ? fmtDate(link.lastAccessedAt) : '없음'),
          ]),
        ]),
        actions,
        badges,
      ]));
    }
  }

  async function toggleLink(link) {
    try {
      await api('PATCH', `/adminpage/api/shorturls/${link.id}`, { enabled: !link.enabled });
      toast(link.enabled ? '비활성화했습니다' : '활성화했습니다');
      loadLinks();
    } catch (e) { showGlobalError(e.message); }
  }

  async function deleteLink(link) {
    const extra = link.qrCount > 0
      ? ` 연결된 QR ${link.qrCount}개와 저장된 이미지도 함께 삭제됩니다.`
      : '';
    const ok = await confirmDialog({
      title: '단축 URL 삭제',
      message: `${link.shortUrl} 을(를) 삭제하면 이 주소로는 더 이상 접속할 수 없습니다.${extra}`,
      word: link.code,
    });
    if (!ok) return;
    try {
      await api('DELETE', `/adminpage/api/shorturls/${link.id}`);
      toast('삭제했습니다');
      loadLinks();
    } catch (e) { showGlobalError(e.message); }
  }

  async function createQRForLink(link) {
    try {
      const data = await api('POST', `/adminpage/api/shorturls/${link.id}/qr`, {
        ecLevel: 'M', size: 512, expiryUsing: 'link',
      });
      toast('QR을 생성했습니다');
      loadLinks();
      showQRDetail(data.item);
    } catch (e) { showGlobalError(e.message); }
  }

  /* ------------------------------------------------------------ QR list */

  const qrSearch = document.getElementById('qr-search');
  const qrKindFilter = document.getElementById('qr-kind');
  const qrStatusFilter = document.getElementById('qr-status');
  const qrSort = document.getElementById('qr-sort');

  // append keeps the already loaded page and adds the next one, which is how
  // the "load more" button walks a library larger than a single page.
  async function loadQRs(append) {
    const params = new URLSearchParams({
      q: qrSearch.value.trim(),
      kind: qrKindFilter.value,
      status: qrStatusFilter.value,
      sort: qrSort.value,
      order: 'desc',
      offset: String(append ? state.qrs.length : 0),
    });
    try {
      const data = await api('GET', `/adminpage/api/qrcodes?${params.toString()}`);
      showGlobalError(null);
      state.qrs = append ? state.qrs.concat(data.items || []) : (data.items || []);
      state.qrTotal = data.total || 0;
      renderQRs();

      document.getElementById('qr-summary').textContent =
        `${num(state.qrs.length)} / ${num(state.qrTotal)}개 표시`;
      const more = document.getElementById('qr-more');
      more.hidden = state.qrs.length >= state.qrTotal;
    } catch (e) {
      showGlobalError(e.message);
    }
  }

  function renderQRs() {
    const grid = document.getElementById('qr-grid');
    clear(grid);
    if (!state.qrs.length) {
      grid.appendChild(el('p', { class: 'empty', text: '조건에 맞는 QR이 없습니다.' }));
      return;
    }
    for (const qr of state.qrs) {
      const thumb = el('div', { class: 'qr-thumb' }, [
        qr.pngUrl ? el('img', { src: qr.pngUrl, alt: `${qr.label || qr.payload} QR 미리보기`, loading: 'lazy' }) : null,
      ]);
      grid.appendChild(el('article', { class: 'qr-card', dataset: { status: qr.status } }, [
        thumb,
        el('div', { class: 'row-badges' }, [
          statusPill(qr.status),
          el('span', { class: 'pill', text: KIND_LABEL[qr.kind] || qr.kind }),
          el('span', { class: qr.revocable ? 'pill pill-active' : 'pill pill-expiring',
            text: qr.revocable ? '만료 시 접속 차단' : '이미지 회수 불가',
            title: qr.revocable
              ? '단축 URL을 거치므로 만료·비활성화하면 실제 접속도 막힙니다.'
              : '내용이 이미지 안에 들어 있어, 배포된 QR은 서버에서 무효화할 수 없습니다.' }),
        ]),
        el('span', { class: 'qr-title', text: qr.label || (qr.shortCode ? `/${qr.shortCode}` : 'QR 코드') }),
        el('span', { class: 'qr-payload', text: qr.payload }),
        el('div', { class: 'qr-meta' }, [
          el('span', { text: `생성 ${fmtDate(qr.createdAt)}` }),
          el('span', { text: `만료 ${qr.expiresAt ? fmtDate(qr.expiresAt) : '없음'} · 남은 시간 ${fmtRemaining(qr.remainingSeconds)}` }),
          el('span', { text: `버전 ${qr.version} · 정정 ${qr.ecLevel} · ${qr.size}px` }),
        ]),
        el('div', { class: 'qr-actions' }, [
          el('a', { class: 'btn btn-sm', href: `${qr.pngUrl}?download=1`, download: '', text: 'PNG' }),
          el('a', { class: 'btn btn-sm', href: `${qr.svgUrl}?download=1`, download: '', text: 'SVG' }),
          el('button', { class: 'btn btn-sm', type: 'button', text: '상세', onclick: () => showQRDetail(qr) }),
          el('button', { class: 'btn btn-sm', type: 'button', text: '만료', onclick: () => openExpiryDialog('qr', qr) }),
          el('button', { class: 'btn btn-sm btn-danger', type: 'button', text: '삭제', onclick: () => deleteQR(qr) }),
        ]),
      ]));
    }
  }

  async function deleteQR(qr) {
    const ok = await confirmDialog({
      title: 'QR 삭제',
      message: qr.revocable
        ? 'QR 기록과 서버에 저장된 PNG/SVG 파일이 삭제됩니다. 연결된 단축 URL은 남아 있습니다.'
        : 'QR 기록과 저장 파일이 삭제됩니다. 이미 내려받은 이미지는 계속 동작한다는 점에 유의하세요.',
    });
    if (!ok) return;
    try {
      await api('DELETE', `/adminpage/api/qrcodes/${encodeURIComponent(qr.id)}`);
      toast('삭제했습니다');
      loadQRs(false);
    } catch (e) { showGlobalError(e.message); }
  }

  /* ------------------------------------------------------- detail modal */

  const detailDialog = document.getElementById('detail-dialog');

  function showQRDetail(qr) {
    document.getElementById('detail-title').textContent = qr.label || KIND_LABEL[qr.kind] || 'QR 상세';
    const body = document.getElementById('detail-body');
    clear(body);
    body.append(
      el('div', { class: 'detail-preview' }, [
        qr.pngUrl ? el('img', { src: qr.pngUrl, alt: 'QR 미리보기' }) : el('span', { text: '이미지 없음' }),
      ]),
      el('p', {
        class: qr.revocable ? 'alert alert-info' : 'alert alert-warn',
        text: qr.revocable
          ? '이 QR은 단축 URL을 담고 있습니다. 만료하거나 비활성화하면 실제 접속도 즉시 차단됩니다.'
          : '이 QR은 내용을 직접 담고 있습니다. 이미 배포된 이미지는 서버에서 무효화할 수 없고, 서버의 기록과 파일만 삭제됩니다.',
      }),
      el('dl', { class: 'detail-grid' }, [
        el('dt', { text: '종류' }), el('dd', { text: KIND_LABEL[qr.kind] || qr.kind }),
        el('dt', { text: '페이로드' }), el('dd', { text: qr.payload }),
        el('dt', { text: '연결 단축 URL' }), el('dd', { text: qr.shortUrl || '없음' }),
        el('dt', { text: '상태' }), el('dd', { text: STATUS_LABEL[qr.status] || qr.status }),
        el('dt', { text: '생성일' }), el('dd', { text: fmtDate(qr.createdAt) }),
        el('dt', { text: '만료일' }), el('dd', { text: qr.expiresAt ? fmtDate(qr.expiresAt) : '없음' }),
        el('dt', { text: '남은 시간' }), el('dd', { text: fmtRemaining(qr.remainingSeconds) }),
        el('dt', { text: '심볼' }), el('dd', { text: `버전 ${qr.version} · 정정 레벨 ${qr.ecLevel} · ${qr.size}px` }),
      ]),
    );

    const foot = document.getElementById('detail-foot');
    clear(foot);
    foot.append(
      el('button', { class: 'btn', type: 'button', text: '페이로드 복사', onclick: () => copyText(qr.payload, '페이로드') }),
      el('a', { class: 'btn', href: `${qr.pngUrl}?download=1`, download: '', text: 'PNG 다운로드' }),
      el('a', { class: 'btn', href: `${qr.svgUrl}?download=1`, download: '', text: 'SVG 다운로드' }),
      el('button', {
        class: 'btn', type: 'button', text: '다시 생성',
        onclick: async () => {
          try {
            const data = await api('POST', `/adminpage/api/qrcodes/${encodeURIComponent(qr.id)}/regenerate`, {});
            toast('다시 생성했습니다');
            detailDialog.close();
            loadQRs(false);
            showQRDetail(data.item);
          } catch (e) { showGlobalError(e.message); }
        },
      }),
      el('button', { class: 'btn btn-primary', type: 'button', text: '닫기', onclick: () => detailDialog.close() }),
    );
    detailDialog.showModal();
  }

  /* -------------------------------------------------- short URL dialog */

  const linkDialog = document.getElementById('link-dialog');
  const linkForm = document.getElementById('link-form');
  let editingLink = null;

  function bindCustomExpiry(selectId, fieldId) {
    const select = document.getElementById(selectId);
    const field = document.getElementById(fieldId);
    const sync = () => { field.hidden = select.value !== 'custom'; };
    select.addEventListener('change', sync);
    sync();
  }
  bindCustomExpiry('link-expiry', 'link-expiry-custom-field');
  bindCustomExpiry('qr-expiry', 'qr-expiry-custom-field');
  bindCustomExpiry('expiry-preset', 'expiry-custom-field');
  document.getElementById('qr-link-expiry').addEventListener('change', function () {
    document.getElementById('qr-link-expiry-custom-field').hidden = this.value !== 'custom';
  });

  document.getElementById('link-create-qr').addEventListener('change', function () {
    document.getElementById('link-qr-options').hidden = !this.checked;
  });

  function openLinkDialog(link) {
    editingLink = link || null;
    document.getElementById('link-dialog-title').textContent = link ? `단축 URL 수정 · /${link.code}` : '새 단축 URL';
    document.getElementById('link-form-error').hidden = true;
    document.getElementById('link-target').value = link ? link.targetUrl : '';
    document.getElementById('link-code').value = link ? link.code : '';
    document.getElementById('link-note').value = link ? link.note : '';
    document.getElementById('link-enabled').checked = link ? link.enabled : true;
    document.getElementById('link-expiry').value = 'none';
    document.getElementById('link-expiry-custom-field').hidden = true;
    document.getElementById('link-create-qr').checked = false;
    document.getElementById('link-qr-options').hidden = true;
    document.getElementById('link-qr-block').hidden = Boolean(link);
    if (link && link.expiresAt) {
      document.getElementById('link-expiry').value = 'custom';
      document.getElementById('link-expiry-custom-field').hidden = false;
      document.getElementById('link-expiry-custom').value = toLocalInput(link.expiresAt);
    }
    linkDialog.showModal();
  }

  function toLocalInput(iso) {
    const parts = dateFormatter().formatToParts(new Date(iso));
    const get = (t) => (parts.find((p) => p.type === t) || {}).value || '00';
    return `${get('year')}-${get('month')}-${get('day')}T${get('hour')}:${get('minute')}`;
  }

  linkForm.addEventListener('submit', async function (event) {
    event.preventDefault();
    const errorBox = document.getElementById('link-form-error');
    errorBox.hidden = true;

    const preset = document.getElementById('link-expiry').value;
    const custom = document.getElementById('link-expiry-custom').value;
    const payload = {
      targetUrl: document.getElementById('link-target').value.trim(),
      code: document.getElementById('link-code').value.trim(),
      note: document.getElementById('link-note').value.trim(),
      enabled: document.getElementById('link-enabled').checked,
      expiryPreset: preset,
      expiryCustom: custom,
    };

    try {
      if (editingLink) {
        const patch = {
          targetUrl: payload.targetUrl,
          note: payload.note,
          enabled: payload.enabled,
          expiryPreset: payload.expiryPreset,
          expiryCustom: payload.expiryCustom,
        };
        // An empty field means "leave the code alone", not "clear it".
        if (payload.code) patch.code = payload.code;
        await api('PATCH', `/adminpage/api/shorturls/${editingLink.id}`, patch);
        toast('수정했습니다');
      } else {
        payload.createQr = document.getElementById('link-create-qr').checked;
        if (payload.createQr) {
          payload.qrEcLevel = document.getElementById('link-qr-ec').value;
          payload.qrSize = parseInt(document.getElementById('link-qr-size').value, 10);
          payload.qrExpiryUsing = 'link';
        }
        const data = await api('POST', '/adminpage/api/shorturls', payload);
        toast(data.qrError ? `단축 URL을 만들었습니다 (QR 실패: ${data.qrError})` : '단축 URL을 만들었습니다');
      }
      linkDialog.close();
      loadLinks();
    } catch (e) {
      errorBox.textContent = e.message;
      errorBox.hidden = false;
    }
  });

  document.getElementById('new-link').addEventListener('click', () => openLinkDialog(null));

  /* --------------------------------------------------------- QR dialog */

  const qrDialog = document.getElementById('qr-dialog');
  const qrFormDialog = document.getElementById('qr-form-dialog');
  const qrKindInputs = Array.from(document.querySelectorAll('input[name="qr-kind-input"]'));

  const KIND_NOTE = {
    short: '단축 URL을 QR에 담습니다. 만료·비활성화하면 실제 접속도 즉시 차단됩니다.',
    url: '입력한 주소가 그대로 QR에 들어갑니다. 이미 배포한 QR은 서버에서 무효화할 수 없습니다.',
    text: '입력한 문구가 그대로 QR에 들어갑니다. 이미 배포한 QR은 서버에서 무효화할 수 없습니다.',
  };

  function currentQRKind() {
    const checked = qrKindInputs.find((i) => i.checked);
    return checked ? checked.value : 'short';
  }

  function syncQRKind() {
    const kind = currentQRKind();
    document.querySelectorAll('[data-kind-block]').forEach((block) => {
      block.hidden = block.dataset.kindBlock !== kind;
    });
    const note = document.getElementById('qr-kind-note');
    clear(note);
    note.className = kind === 'short' ? 'alert alert-info' : 'alert alert-warn';
    note.appendChild(document.createTextNode(KIND_NOTE[kind]));

    // A Direct URL QR can be turned into a revocable one in a single click,
    // so offer that instead of only warning about the limitation.
    if (kind === 'url') {
      note.appendChild(document.createTextNode(' '));
      note.appendChild(el('button', {
        class: 'btn btn-sm', type: 'button', text: '취소 가능한 Short URL QR로 바꾸기',
        onclick: () => {
          const url = document.getElementById('qr-url').value.trim();
          qrKindInputs.find((i) => i.value === 'short').checked = true;
          syncQRKind();
          document.getElementById('qr-short-code').value = '';
          document.getElementById('qr-new-short').hidden = false;
          if (url) document.getElementById('qr-target').value = url;
          refreshDialogPreview();
        },
      }));
    }
    if (typeof refreshDialogPreview === 'function') refreshDialogPreview();
  }
  qrKindInputs.forEach((i) => i.addEventListener('change', syncQRKind));

  document.getElementById('qr-short-code').addEventListener('change', function () {
    document.getElementById('qr-new-short').hidden = this.value !== '';
  });

  async function openQRDialog() {
    document.getElementById('qr-form-error').hidden = true;
    qrFormDialog.reset();
    syncQRKind();
    document.getElementById('qr-expiry-custom-field').hidden = true;
    document.getElementById('qr-link-expiry-custom-field').hidden = true;
    document.getElementById('qr-new-short').hidden = false;

    const select = document.getElementById('qr-short-code');
    clear(select);
    select.appendChild(el('option', { value: '', text: '새 단축 URL 만들기' }));
    try {
      const data = await api('GET', '/adminpage/api/shorturls?sort=code&order=asc');
      for (const link of data.items || []) {
        select.appendChild(el('option', {
          value: link.code,
          text: `/${link.code} → ${link.targetUrl}`,
        }));
      }
    } catch (e) { /* the dialog still works with a new short URL */ }
    refreshDialogPreview();
    qrDialog.showModal();
  }

  document.getElementById('new-qr').addEventListener('click', openQRDialog);

  const dialogPreview = document.getElementById('qr-dialog-preview');
  const dialogPreviewEmpty = document.getElementById('qr-dialog-preview-empty');
  const dialogPayloadOut = document.getElementById('qr-dialog-payload');
  let previewTimer = null;

  // plannedPayload mirrors what the server will encode, so the preview is the
  // real thing rather than an approximation.
  function plannedPayload() {
    const kind = currentQRKind();
    if (kind === 'url') return document.getElementById('qr-url').value.trim();
    if (kind === 'text') return document.getElementById('qr-text').value;
    const existing = document.getElementById('qr-short-code').value;
    if (existing) return `${BASE_URL}/${existing}`;
    const wanted = document.getElementById('qr-new-code').value.trim();
    if (!document.getElementById('qr-target').value.trim()) return '';
    if (!/^[0-9]{1,3}$/.test(wanted)) return `${BASE_URL}/### (자동 할당)`;
    return `${BASE_URL}/${wanted.padStart(3, '0')}`;
  }

  function refreshDialogPreview() {
    const payload = plannedPayload();
    dialogPayloadOut.textContent = payload;
    if (!payload || payload.includes('###')) {
      dialogPreview.hidden = true;
      dialogPreview.removeAttribute('src');
      dialogPreviewEmpty.hidden = false;
      dialogPreviewEmpty.textContent = payload
        ? '번호는 저장할 때 자동으로 정해집니다.'
        : '내용을 입력하면 미리보기가 나타납니다.';
      return;
    }
    const params = new URLSearchParams({
      data: payload,
      ec: document.getElementById('qr-ec').value,
      size: '512',
    });
    dialogPreviewEmpty.hidden = true;
    dialogPreview.hidden = false;
    dialogPreview.src = `/adminpage/qr/preview.png?${params.toString()}`;
  }

  function scheduleDialogPreview() {
    window.clearTimeout(previewTimer);
    previewTimer = window.setTimeout(refreshDialogPreview, 200);
  }

  dialogPreview.addEventListener('error', function () {
    dialogPreview.hidden = true;
    dialogPreviewEmpty.hidden = false;
    dialogPreviewEmpty.textContent = '이 내용으로는 QR을 만들 수 없습니다.';
  });

  ['qr-url', 'qr-text', 'qr-target', 'qr-new-code'].forEach((id) => {
    document.getElementById(id).addEventListener('input', scheduleDialogPreview);
  });
  document.getElementById('qr-ec').addEventListener('change', refreshDialogPreview);
  document.getElementById('qr-short-code').addEventListener('change', refreshDialogPreview);
  qrKindInputs.forEach((i) => i.addEventListener('change', refreshDialogPreview));

  qrFormDialog.addEventListener('submit', async function (event) {
    event.preventDefault();
    const errorBox = document.getElementById('qr-form-error');
    errorBox.hidden = true;

    const kind = currentQRKind();
    const body = {
      kind,
      label: document.getElementById('qr-label').value.trim(),
      ecLevel: document.getElementById('qr-ec').value,
      size: parseInt(document.getElementById('qr-size').value, 10),
      expiryPreset: document.getElementById('qr-expiry').value,
      expiryCustom: document.getElementById('qr-expiry-custom').value,
    };
    if (kind === 'url') body.payload = document.getElementById('qr-url').value.trim();
    if (kind === 'text') body.payload = document.getElementById('qr-text').value;
    if (kind === 'short') {
      const existing = document.getElementById('qr-short-code').value;
      if (existing) {
        body.shortCode = existing;
      } else {
        body.targetUrl = document.getElementById('qr-target').value.trim();
        body.newCode = document.getElementById('qr-new-code').value.trim();
        const linkExpiry = document.getElementById('qr-link-expiry').value;
        if (linkExpiry === '__qr') {
          body.shortExpiryUsing = 'qr';
        } else {
          body.shortExpiryPreset = linkExpiry;
          body.shortExpiryCustom = document.getElementById('qr-link-expiry-custom').value;
        }
      }
    }

    try {
      const data = await api('POST', '/adminpage/api/qrcodes', body);
      qrDialog.close();
      toast('QR을 생성했습니다');
      loadQRs(false);
      showQRDetail(data.item);
    } catch (e) {
      errorBox.textContent = e.message;
      errorBox.hidden = false;
    }
  });

  /* ----------------------------------------------------- expiry dialog */

  const expiryDialog = document.getElementById('expiry-dialog');
  const expiryForm = document.getElementById('expiry-form');
  let expiryTarget = null;

  document.getElementById('expiry-mode').addEventListener('change', function () {
    const extend = this.value === 'extend';
    document.getElementById('expiry-set-field').hidden = extend;
    document.getElementById('expiry-extend-field').hidden = !extend;
    document.getElementById('expiry-custom-field').hidden =
      extend || document.getElementById('expiry-preset').value !== 'custom';
  });

  function openExpiryDialog(type, item) {
    expiryTarget = { type, item };
    document.getElementById('expiry-form-error').hidden = true;
    document.getElementById('expiry-dialog-title').textContent =
      type === 'link' ? `만료 관리 · /${item.code}` : '만료 관리 · QR';
    document.getElementById('expiry-subject').textContent =
      type === 'link' ? item.shortUrl : (item.label || item.payload);
    document.getElementById('expiry-mode').value = 'set';
    document.getElementById('expiry-set-field').hidden = false;
    document.getElementById('expiry-extend-field').hidden = true;
    document.getElementById('expiry-preset').value = item.expiresAt ? 'custom' : 'none';
    document.getElementById('expiry-custom-field').hidden = !item.expiresAt;
    document.getElementById('expiry-custom').value = item.expiresAt ? toLocalInput(item.expiresAt) : '';
    expiryDialog.showModal();
  }

  expiryForm.addEventListener('submit', async function (event) {
    event.preventDefault();
    if (!expiryTarget) return;
    const errorBox = document.getElementById('expiry-form-error');
    errorBox.hidden = true;

    const { type, item } = expiryTarget;
    const base = type === 'link'
      ? `/adminpage/api/shorturls/${item.id}`
      : `/adminpage/api/qrcodes/${encodeURIComponent(item.id)}`;
    const mode = document.getElementById('expiry-mode').value;

    try {
      if (mode === 'extend') {
        await api('POST', `${base}/extend`, { duration: document.getElementById('expiry-extend').value });
      } else {
        await api('PATCH', base, {
          expiryPreset: document.getElementById('expiry-preset').value,
          expiryCustom: document.getElementById('expiry-custom').value,
        });
      }
      expiryDialog.close();
      toast('만료 기간을 변경했습니다');
      if (type === 'link') loadLinks(); else loadQRs(false);
    } catch (e) {
      errorBox.textContent = e.message;
      errorBox.hidden = false;
    }
  });

  /* ---------------------------------------------------- confirm dialog */

  const confirmDialogEl = document.getElementById('confirm-dialog');
  const confirmForm = document.getElementById('confirm-form');
  let confirmResolve = null;
  let confirmWord = '';
  let confirmAccepted = false;

  function confirmDialog({ title, message, word }) {
    document.getElementById('confirm-title').textContent = title;
    document.getElementById('confirm-message').textContent = message;
    confirmWord = word || '';
    confirmAccepted = false;
    const field = document.getElementById('confirm-typing-field');
    field.hidden = !confirmWord;
    document.getElementById('confirm-word').textContent = confirmWord;
    document.getElementById('confirm-input').value = '';
    confirmDialogEl.showModal();
    return new Promise((resolve) => { confirmResolve = resolve; });
  }

  confirmForm.addEventListener('submit', function (event) {
    event.preventDefault();
    if (confirmWord && document.getElementById('confirm-input').value.trim() !== confirmWord) {
      toast(`${confirmWord} 을(를) 정확히 입력해 주세요`);
      return;
    }
    confirmAccepted = true;
    confirmDialogEl.close();
  });

  // The close event covers every exit: the submit above, the cancel button and
  // the Escape key.
  confirmDialogEl.addEventListener('close', function () {
    const resolve = confirmResolve;
    confirmResolve = null;
    const accepted = confirmAccepted;
    confirmAccepted = false;
    if (resolve) resolve(accepted);
  });

  /* ---------------------------------------------------------- settings */

  const EXPORTS = [
    { kind: 'shorturls', label: '단축 URL' },
    { kind: 'qrcodes', label: 'QR 목록' },
    { kind: 'audit', label: '감사 로그' },
  ];

  function definition(list, term, value) {
    list.append(el('dt', { text: term }), el('dd', { text: value }));
  }

  function renderExportLinks() {
    const box = document.getElementById('export-links');
    clear(box);
    for (const item of EXPORTS) {
      for (const format of ['csv', 'json']) {
        box.appendChild(el('a', {
          class: 'btn btn-sm',
          // A download is a plain navigation, so the token travels in the URL.
          href: `/adminpage/api/export/${item.kind}?format=${format}&csrf=${encodeURIComponent(CSRF)}`,
          download: '',
          text: `${item.label} ${format.toUpperCase()}`,
        }));
      }
    }
  }

  function renderAudit(entries) {
    const list = document.getElementById('audit-list');
    clear(list);
    if (!entries.length) {
      list.appendChild(el('p', { class: 'empty', text: '기록이 없습니다.' }));
      return;
    }
    for (const e of entries) {
      list.appendChild(el('div', { class: e.success ? 'audit-row' : 'audit-row is-failure' }, [
        el('span', { class: 'audit-time', text: fmtDate(e.createdAt) }),
        el('span', { class: 'audit-action', text: e.action }),
        el('span', { class: 'audit-detail', text: [e.subject, e.detail].filter(Boolean).join(' · ') }),
        el('span', { class: 'audit-ip', text: e.ip }),
      ]));
    }
  }

  async function loadAccount() {
    try {
      const data = await api('GET', '/adminpage/api/account');
      showGlobalError(null);
      if (!state.timezone) state.timezone = data.diagnostics.timezone;

      const summary = document.getElementById('account-summary');
      clear(summary);
      definition(summary, '아이디', data.username);
      definition(summary, '계정 생성', fmtDate(data.createdAt));
      definition(summary, '비밀번호 변경', fmtDate(data.passwordChangedAt));
      definition(summary, '현재 세션 만료', fmtDate(data.sessionExpiresAt));
      definition(summary, '활성 세션', `${num(data.activeSessions)}개`);
      definition(summary, '24시간 내 로그인 실패', `${num(data.failedLogins24h)}회`);

      const diag = document.getElementById('diagnostics');
      clear(diag);
      const d = data.diagnostics;
      definition(diag, '버전', d.version);
      definition(diag, 'SQLite', d.sqlite);
      definition(diag, '스키마', String(d.schemaVersion));
      definition(diag, '시간대', d.timezone);
      definition(diag, '가동 시간', fmtRemaining(d.uptimeSeconds));
      definition(diag, '공개 QR 생성', d.publicQr ? '켜짐' : '꺼짐');
      definition(diag, '신뢰 프록시', d.trustedProxies);
      definition(diag, 'QR 저장 위치', d.qrDirectory);

      renderExportLinks();
      renderAudit(data.audit || []);
      document.getElementById('audit-summary').textContent =
        `최근 ${data.audit.length}건 · 180일 보관`;
    } catch (e) {
      showGlobalError(e.message);
    }
  }

  document.getElementById('password-form').addEventListener('submit', async function (event) {
    event.preventDefault();
    const errorBox = document.getElementById('password-error');
    const okBox = document.getElementById('password-ok');
    errorBox.hidden = true;
    okBox.hidden = true;

    const current = document.getElementById('current-password');
    const next = document.getElementById('new-password');
    const confirm = document.getElementById('confirm-password');

    if (next.value !== confirm.value) {
      errorBox.textContent = '새 비밀번호가 서로 일치하지 않습니다.';
      errorBox.hidden = false;
      return;
    }
    try {
      const res = await api('POST', '/adminpage/api/account/password', {
        currentPassword: current.value,
        newPassword: next.value,
        confirmPassword: confirm.value,
      });
      // The server rotated the session, so adopt the new CSRF token.
      if (res.csrfToken) CSRF = res.csrfToken;
      current.value = next.value = confirm.value = '';
      okBox.textContent = `비밀번호를 변경했습니다. 다른 세션 ${res.revokedSessions}개가 로그아웃되었습니다.`;
      okBox.hidden = false;
      toast('비밀번호를 변경했습니다');
      loadAccount();
    } catch (e) {
      errorBox.textContent = e.message;
      errorBox.hidden = false;
    }
  });

  document.getElementById('revoke-sessions').addEventListener('click', async function () {
    const ok = await confirmDialog({
      title: '다른 세션 로그아웃',
      message: '이 브라우저를 제외한 모든 세션이 즉시 로그아웃됩니다.',
    });
    if (!ok) return;
    try {
      const res = await api('POST', '/adminpage/api/account/sessions/revoke', {});
      toast(`${res.revokedSessions}개 세션을 로그아웃했습니다`);
      loadAccount();
    } catch (e) { showGlobalError(e.message); }
  });

  /* -------------------------------------------------------------- misc */

  document.querySelectorAll('[data-close]').forEach((btn) => {
    btn.addEventListener('click', function () {
      const dialog = btn.closest('dialog');
      if (dialog) dialog.close();
    });
  });

  let searchTimer = null;
  function debounceReload(fn) {
    return function () {
      window.clearTimeout(searchTimer);
      searchTimer = window.setTimeout(fn, 220);
    };
  }
  linkSearch.addEventListener('input', debounceReload(loadLinks));
  linkStatus.addEventListener('change', loadLinks);
  linkSort.addEventListener('change', loadLinks);
  linkOrder.addEventListener('click', function () {
    const next = linkOrder.dataset.order === 'asc' ? 'desc' : 'asc';
    linkOrder.dataset.order = next;
    linkOrder.textContent = next === 'asc' ? '↑' : '↓';
    loadLinks();
  });

  const reloadQRs = () => loadQRs(false);
  qrSearch.addEventListener('input', debounceReload(reloadQRs));
  qrKindFilter.addEventListener('change', reloadQRs);
  qrStatusFilter.addEventListener('change', reloadQRs);
  qrSort.addEventListener('change', reloadQRs);
  document.getElementById('qr-more').addEventListener('click', () => loadQRs(true));

  document.getElementById('run-cleanup').addEventListener('click', async function () {
    try {
      const res = await api('POST', '/adminpage/api/cleanup', {});
      toast(res.skipped
        ? '다른 정리 작업이 실행 중입니다'
        : `정리 완료 · URL ${res.shortURLs}개 · QR ${res.qrCodes}개 · 파일 ${res.files + res.orphans}개`);
      activate(document.querySelector('.tab.is-active').dataset.tab);
    } catch (e) { showGlobalError(e.message); }
  });

  const initial = window.location.hash.slice(1);
  const startTab = ['dashboard', 'links', 'qrs', 'settings'].includes(initial) ? initial : 'dashboard';
  // The overview also carries the timezone every date on the page is formatted
  // with, so fetch it even when another tab is opened first.
  if (startTab !== 'dashboard') loadOverview();
  activate(startTab);
  syncQRKind();
})();
