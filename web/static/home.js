/* Public QR generator: builds the preview URL and keeps the download links in
   sync. Nothing is stored on the server, so this is pure query string work. */
(function () {
  'use strict';

  const root = document.querySelector('.generator');
  if (!root) return;

  const maxPayload = parseInt(root.dataset.maxPayload || '2953', 10);
  const content = document.getElementById('content');
  const counter = document.getElementById('content-count');
  const ec = document.getElementById('ec');
  const size = document.getElementById('size');
  const preview = document.getElementById('qr-preview');
  const empty = document.getElementById('preview-empty');
  const error = document.getElementById('preview-error');
  const payloadOut = document.getElementById('preview-payload');
  const pngLink = document.getElementById('download-png');
  const svgLink = document.getElementById('download-svg');
  const copyBtn = document.getElementById('copy-payload');
  const kindInputs = Array.from(document.querySelectorAll('input[name="kind"]'));

  const byteLength = (s) => new TextEncoder().encode(s).length;

  function currentKind() {
    const checked = kindInputs.find((i) => i.checked);
    return checked ? checked.value : 'url';
  }

  function validate(value) {
    if (!value) return '내용을 입력하면 QR이 나타납니다.';
    if (byteLength(value) > maxPayload) return `내용이 너무 깁니다. 최대 ${maxPayload}바이트까지 담을 수 있습니다.`;
    if (currentKind() === 'url' && !/^https?:\/\/[^\s]+$/i.test(value)) {
      return 'http:// 또는 https:// 로 시작하는 주소를 입력해 주세요.';
    }
    return null;
  }

  function buildURL(ext, download) {
    const params = new URLSearchParams();
    params.set('data', content.value);
    params.set('ec', ec.value);
    params.set('size', size.value);
    if (download) {
      params.set('download', '1');
      params.set('name', 'qrcode');
    }
    return `/qr.${ext}?${params.toString()}`;
  }

  let timer = null;
  function refresh() {
    const value = content.value;
    counter.textContent = String(byteLength(value));

    const problem = validate(value);
    if (problem) {
      preview.hidden = true;
      preview.removeAttribute('src');
      payloadOut.textContent = '-';
      pngLink.setAttribute('aria-disabled', 'true');
      svgLink.setAttribute('aria-disabled', 'true');
      pngLink.removeAttribute('href');
      svgLink.removeAttribute('href');
      if (value) {
        error.textContent = problem;
        error.hidden = false;
        empty.hidden = true;
      } else {
        error.hidden = true;
        empty.hidden = false;
      }
      return;
    }

    error.hidden = true;
    empty.hidden = true;
    preview.hidden = false;
    preview.src = buildURL('png', false);
    payloadOut.textContent = value;
    pngLink.href = buildURL('png', true);
    svgLink.href = buildURL('svg', true);
    pngLink.removeAttribute('aria-disabled');
    svgLink.removeAttribute('aria-disabled');
  }

  function schedule() {
    window.clearTimeout(timer);
    timer = window.setTimeout(refresh, 180);
  }

  content.addEventListener('input', schedule);
  ec.addEventListener('change', refresh);
  size.addEventListener('change', refresh);
  kindInputs.forEach((i) => i.addEventListener('change', refresh));

  preview.addEventListener('error', function () {
    preview.hidden = true;
    empty.hidden = true;
    error.textContent = 'QR을 만들지 못했습니다. 내용이 너무 길거나 형식이 올바르지 않습니다.';
    error.hidden = false;
  });

  [pngLink, svgLink].forEach((link) => {
    link.addEventListener('click', function (event) {
      if (link.getAttribute('aria-disabled') === 'true') event.preventDefault();
    });
  });

  copyBtn.addEventListener('click', async function () {
    if (!content.value) return;
    try {
      await navigator.clipboard.writeText(content.value);
      copyBtn.textContent = '복사됨';
    } catch (err) {
      copyBtn.textContent = '복사 실패';
    }
    window.setTimeout(() => { copyBtn.textContent = '페이로드 복사'; }, 1500);
  });

  document.getElementById('qr-form').addEventListener('submit', (e) => e.preventDefault());

  refresh();
})();
