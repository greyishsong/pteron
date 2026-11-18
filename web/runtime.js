(() => {
  'use strict';
  const script = document.currentScript;
  const siteId = script?.getAttribute('data-site-id') || location.pathname.match(/^\/s\/([\w-]+)/)?.[1];
  if (!siteId || window.pteron) return;
  const endpoint = '/api/sites/' + encodeURIComponent(siteId) + '/feedback';
  const ignoredTypes = new Set(['submit', 'button', 'reset', 'file']);
  let isHydrating = false;
  let savedFields = null;
  let revision = 0;
  let savedRevision = 0;
  let timer;
  let queue = Promise.resolve();
  const hydrated = new WeakSet();
  const touched = new Set();
  const keyOf = el => el.getAttribute('data-field') || el.getAttribute('name') || el.id;
  const isField = el => el instanceof Element &&
    el.matches('input, textarea, select, [contenteditable="true"][data-field]') &&
    !ignoredTypes.has(el.type) && !el.closest('[data-pteron-ignore]') && !!keyOf(el);

  function groups() {
    const result = new Map();
    document.querySelectorAll('input, textarea, select, [contenteditable="true"][data-field]').forEach(el => {
      if (!isField(el)) return;
      const key = keyOf(el);
      if (!result.has(key)) result.set(key, []);
      result.get(key).push(el);
    });
    return result;
  }
  function collectFields() {
    const fields = Object.create(null);
    for (const [key, elements] of groups()) {
      const el = elements[0];
      if (el.type === 'checkbox') {
        fields[key] = elements.length > 1 ? elements.filter(e => e.checked).map(e => e.value) : el.checked;
      } else if (el.type === 'radio') {
        fields[key] = elements.find(e => e.checked)?.value ?? null;
      } else if (el.tagName === 'SELECT' && el.multiple) {
        fields[key] = Array.from(el.selectedOptions, option => option.value);
      } else if (el.type === 'number' || el.type === 'range') {
        fields[key] = el.value === '' ? null : Number(el.value);
      } else if (el.isContentEditable) {
        fields[key] = el.innerText ?? el.textContent;
      } else {
        fields[key] = el.value;
      }
    }
    return fields;
  }
  function setProperty(el, property, value) {
    if (el[property] === value) return false;
    // Use native setters so controlled inputs can observe a real change event.
    const setter = Object.getOwnPropertyDescriptor(Object.getPrototypeOf(el), property)?.set;
    if (setter) setter.call(el, value);
    else el[property] = value;
    return true;
  }
  function hydrate() {
    if (!savedFields) return;
    isHydrating = true;
    try {
      for (const [key, elements] of groups()) {
        if (!Object.prototype.hasOwnProperty.call(savedFields, key) || touched.has(key)) continue;
        const value = savedFields[key];
        for (const el of elements) {
          if (hydrated.has(el)) continue;
          hydrated.add(el);
          let changed = false;
          if (el.type === 'checkbox') {
            changed = setProperty(el, 'checked', Array.isArray(value) ? value.map(String).includes(el.value) : !!value);
          } else if (el.type === 'radio') {
            changed = setProperty(el, 'checked', value !== null && String(value) === el.value);
          } else if (el.tagName === 'SELECT' && el.multiple) {
            const values = Array.isArray(value) ? value.map(String) : [];
            for (const option of el.options) changed = setProperty(option, 'selected', values.includes(option.value)) || changed;
          } else if (el.isContentEditable) {
            changed = setProperty(el, 'textContent', value == null ? '' : String(value));
          } else {
            changed = setProperty(el, 'value', value == null ? '' : String(value));
          }
          if (changed) {
            el.dispatchEvent(new Event('input', { bubbles: true }));
            el.dispatchEvent(new Event('change', { bubbles: true }));
          }
        }
      }
    } finally { isHydrating = false; }
  }
  async function getFeedback() {
    const response = await fetch(endpoint, { cache: 'no-store' });
    if (!response.ok) throw new Error('Feedback request failed: ' + response.status);
    return response.json();
  }
  function persist(extra, submitted = false) {
    clearTimeout(timer);
    const atRevision = revision;
    const payload = { fields: collectFields() };
    if (extra !== undefined) payload.extra = extra;
    if (submitted) payload.submitted = true;
    const operation = queue.catch(() => {}).then(async () => {
      const response = await fetch(endpoint, {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload), keepalive: true
      });
      if (!response.ok) throw new Error('Feedback save failed: ' + response.status);
      const feedback = await response.json();
      savedRevision = Math.max(savedRevision, atRevision);
      return feedback;
    });
    queue = operation;
    return operation;
  }
  function autosave() {
    persist().catch(error => {
      console.warn('[pteron]', error);
      if (revision > savedRevision) timer = setTimeout(autosave, 2000);
    });
  }
  function onEdit(event) {
    if (isHydrating) return;
    const el = event.target.closest?.('input, textarea, select, [contenteditable="true"][data-field]');
    if (!isField(el)) return;
    touched.add(keyOf(el));
    revision++;
    clearTimeout(timer);
    timer = setTimeout(autosave, 300);
  }
  function beacon() {
    if (revision <= savedRevision) return;
    clearTimeout(timer);
    const body = JSON.stringify({ fields: collectFields() });
    if (navigator.sendBeacon && navigator.sendBeacon(endpoint, new Blob([body], { type: 'application/json' }))) return;
    fetch(endpoint, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body, keepalive: true })
      .catch(error => console.warn('[pteron]', error));
  }
  function toast(message) {
    const el = document.createElement('div');
    el.setAttribute('data-pteron-ignore', '');
    el.setAttribute('role', 'status');
    el.textContent = message;
    Object.assign(el.style, {
      position: 'fixed', bottom: '20px', right: '20px', zIndex: '2147483647',
      padding: '9px 16px', borderRadius: '20px', color: '#fff', background: '#263f52',
      font: '14px system-ui, sans-serif', boxShadow: '0 2px 12px #0002', pointerEvents: 'none'
    });
    document.body.append(el);
    setTimeout(() => el.remove(), 2000);
  }
  async function submit(extra) {
    const feedback = await persist(extra, true);
    toast('反馈已保存');
    return feedback;
  }
  function submitFromEvent(event) {
    event.preventDefault();
    submit().catch(error => { console.warn('[pteron]', error); toast('保存失败，请重试'); });
  }
  window.pteron = { save: extra => persist(extra), submit, getFields: collectFields, getFeedback };
  document.addEventListener('input', onEdit);
  document.addEventListener('change', onEdit);
  document.addEventListener('submit', event => {
    const form = event.target;
    if (!(form instanceof HTMLFormElement) || form.closest('[data-pteron-ignore]')) return;
    const action = event.submitter?.getAttribute('formaction') ?? form.getAttribute('action');
    if (action && new URL(action, location.href).origin !== location.origin) return;
    submitFromEvent(event);
  });
  document.addEventListener('click', event => {
    const button = event.target.closest?.('[data-pteron-submit]');
    if (button && !button.closest('[data-pteron-ignore]')) submitFromEvent(event);
  });
  document.addEventListener('visibilitychange', () => { if (document.visibilityState === 'hidden') beacon(); });
  window.addEventListener('pagehide', beacon);

  function start() {
    const observer = new MutationObserver(hydrate);
    observer.observe(document.documentElement, { childList: true, subtree: true });
    setTimeout(() => observer.disconnect(), 2000);
    getFeedback().then(feedback => { savedFields = feedback.fields; hydrate(); })
      .catch(error => console.warn('[pteron]', error));
    const events = new EventSource('/api/sites/' + encodeURIComponent(siteId) + '/events');
    events.addEventListener('reload', async () => {
      try {
        if (revision > savedRevision) await persist();
        else await queue;
        location.reload();
      } catch (error) { console.warn('[pteron] Save failed; reload postponed.', error); }
    });
    window.addEventListener('pagehide', () => events.close(), { once: true });
  }
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', start, { once: true });
  else start();
})();
