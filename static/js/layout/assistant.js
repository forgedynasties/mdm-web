// The assistant bubble (C3, picked 25 Nov 2026; server: internal/dashboard/assistant.go).
// Loaded by layout.html for super admins while the assistant is configured. The bubble and
// its panel live in the page chrome, so they stay put across in-app navigation; the
// conversation (its id, and what is on screen) is kept for the tab in sessionStorage.
// Answers stream from POST /assistant/chat as server-sent events read off fetch().
(function () {
  'use strict';
  if (window.__mdmAssistant) return; window.__mdmAssistant = true;
  var KEY_CONV = 'mdm-as-conv', KEY_LOG = 'mdm-as-log';
  function load(k, d) { try { var v = sessionStorage.getItem(k); return v ? JSON.parse(v) : d; } catch (e) { return d; } }
  function save(k, v) { try { sessionStorage.setItem(k, JSON.stringify(v)); } catch (e) {} }

  var conv = load(KEY_CONV, ''), log = load(KEY_LOG, []), busy = null;

  var root = document.createElement('div');
  root.id = 'as-root';
  root.innerHTML =
    '<button type="button" class="as-bub" id="as-bub" aria-label="Ask the assistant" aria-expanded="false" title="Ask the assistant (Ctrl+J)">' +
    '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 3l1.9 4.6L18.5 9.5l-4.6 1.9L12 16l-1.9-4.6L5.5 9.5l4.6-1.9z"/><path d="M19 15l.8 1.9 1.9.8-1.9.8L19 20.5l-.8-1.9-1.9-.8 1.9-.8z"/></svg></button>' +
    '<section class="as-pan" id="as-pan" role="dialog" aria-label="Assistant" hidden>' +
    '<header class="as-hd"><span class="as-av" aria-hidden="true">✦</span><div class="as-ht"><b>Assistant</b><small>Read-only · looks things up as you</small></div>' +
    '<button type="button" class="as-ib" id="as-new" title="New conversation" aria-label="New conversation"><svg viewBox="0 0 24 24"><path d="M12 5v14M5 12h14"/></svg></button>' +
    '<button type="button" class="as-ib" id="as-x" title="Close (Esc)" aria-label="Close"><svg viewBox="0 0 24 24"><path d="M18 6 6 18M6 6l12 12"/></svg></button></header>' +
    '<div class="as-body" id="as-body" aria-live="polite"></div>' +
    '<form class="as-foot" id="as-form"><div class="as-ctx" id="as-ctx"></div><div class="as-inp">' +
    '<textarea id="as-q" rows="1" maxlength="2000" placeholder="Ask about devices, venues, alerts…" aria-label="Your question"></textarea>' +
    '<button type="submit" class="as-go" id="as-go" aria-label="Send"><svg viewBox="0 0 24 24"><path d="M12 19V5M5 12l7-7 7 7"/></svg></button></div></form></section>';
  document.body.appendChild(root);

  var bub = root.querySelector('#as-bub'), pan = root.querySelector('#as-pan'), body = root.querySelector('#as-body'),
      form = root.querySelector('#as-form'), q = root.querySelector('#as-q'), go = root.querySelector('#as-go'), ctxEl = root.querySelector('#as-ctx');

  function esc(s) { return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) { return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]; }); }
  // A small Markdown subset, escaped first: **bold**, `code` (a serial becomes a link to its
  // device), "- " lists, "1. " lists, paragraphs. Nothing else is turned into HTML.
  function inline(s) {
    return esc(s).replace(/\*\*([^*]+)\*\*/g, '<b>$1</b>').replace(/`([^`]+)`/g, function (_, c) {
      return /^[A-Za-z0-9][A-Za-z0-9_-]{5,40}$/.test(c) ? '<a class="as-ser" href="/devices/' + encodeURIComponent(c) + '">' + c + '</a>' : '<code>' + c + '</code>';
    });
  }
  function md(text) {
    var out = [], list = null;
    function close() { if (list) { out.push('</' + list + '>'); list = null; } }
    String(text || '').split(/\n/).forEach(function (line) {
      var m;
      if ((m = line.match(/^\s*[-*•]\s+(.*)$/))) { if (list !== 'ul') { close(); out.push('<ul>'); list = 'ul'; } out.push('<li>' + inline(m[1]) + '</li>'); return; }
      if ((m = line.match(/^\s*\d+[.)]\s+(.*)$/))) { if (list !== 'ol') { close(); out.push('<ol>'); list = 'ol'; } out.push('<li>' + inline(m[1]) + '</li>'); return; }
      close();
      if (line.trim() === '') { out.push('<br>'); return; }
      out.push('<p>' + inline(line.replace(/^#+\s*/, '')) + '</p>');
    });
    close();
    return out.join('').replace(/(<br>)+/g, '').replace(/^<br>|<br>$/g, '');
  }

  function pageCtx() {
    var p = location.pathname, m;
    if ((m = p.match(/^\/devices\/([^/]+)$/))) return { label: 'Looking at <b>' + esc(decodeURIComponent(m[1])) + '</b>', kind: 'device' };
    if (/^\/restaurants\/[0-9a-f-]{36}/.test(p)) { var h = document.querySelector('h1'); return { label: 'On the venue <b>' + esc(h ? h.textContent.trim() : '') + '</b>', kind: 'venue' }; }
    if (/^\/alerts/.test(p)) return { label: 'On <b>Alerts</b>', kind: 'alerts' };
    return { label: '', kind: '' };
  }
  function suggestions(kind) {
    if (kind === 'device') return ['What\'s wrong with this device?', 'What was done to it this week?', 'Is it running hot?', 'Other devices at its venue'];
    if (kind === 'venue') return ['Which devices here are offline?', 'Open alerts at this venue', 'Summarise this venue'];
    if (kind === 'alerts') return ['Which venues have the most open alerts?', 'Explain the critical alerts'];
    return ['Which devices are offline right now?', 'Which venues have open alerts?', 'Devices still on an old client', 'List the venues'];
  }

  function render() {
    var c = pageCtx();
    ctxEl.innerHTML = c.label;
    ctxEl.hidden = !c.label;
    if (!log.length) {
      body.innerHTML = '<div class="as-empty"><p>Ask about any device, venue or alert. I look it up as you, and I can\'t change anything.</p><div class="as-sugg">' +
        suggestions(c.kind).map(function (s) { return '<button type="button" data-ask="' + esc(s) + '">' + esc(s) + '</button>'; }).join('') + '</div></div>';
      return;
    }
    body.innerHTML = log.map(function (m) {
      if (m.role === 'user') return '<div class="as-u">' + esc(m.text) + '</div>';
      var tools = (m.tools || []).map(function (t) { return '<span class="as-tool' + (t.done ? '' : ' run') + '"><i></i>' + esc(t.label) + (t.done && t.summary ? ' · ' + esc(t.summary) : t.done ? '' : '…') + '</span>'; }).join('');
      return '<div class="as-a' + (m.error ? ' err' : '') + '">' + (tools ? '<div class="as-tools">' + tools + '</div>' : '') +
        (m.text ? '<div class="as-txt' + (m.live ? ' live' : '') + '">' + md(m.text) + '</div>' : (m.live && !tools ? '<div class="as-txt live"><span class="as-dots"><i></i><i></i><i></i></span></div>' : '')) +
        (m.error ? '<p class="as-errt">' + esc(m.error) + '</p>' : '') + '</div>';
    }).join('');
    body.scrollTop = body.scrollHeight;
  }

  function open(on) {
    pan.hidden = !on; bub.setAttribute('aria-expanded', on ? 'true' : 'false'); bub.classList.toggle('on', on);
    if (on) { render(); setTimeout(function () { q.focus(); }, 30); }
  }
  bub.addEventListener('click', function () { open(pan.hidden); });
  root.querySelector('#as-x').addEventListener('click', function () { open(false); });
  root.querySelector('#as-new').addEventListener('click', function () {
    if (busy) busy.abort();
    conv = ''; log = []; save(KEY_CONV, conv); save(KEY_LOG, log); render(); q.focus();
  });
  document.addEventListener('keydown', function (e) {
    if ((e.ctrlKey || e.metaKey) && (e.key === 'j' || e.key === 'J')) { e.preventDefault(); open(pan.hidden); return; }
    if (e.key === 'Escape' && !pan.hidden && pan.contains(document.activeElement)) { e.stopPropagation(); open(false); bub.focus(); }
  }, true);
  body.addEventListener('click', function (e) { var b = e.target.closest('[data-ask]'); if (b) ask(b.getAttribute('data-ask')); });
  q.addEventListener('input', function () { q.style.height = 'auto'; q.style.height = Math.min(q.scrollHeight, 140) + 'px'; });
  q.addEventListener('keydown', function (e) { if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); form.requestSubmit(); } });
  form.addEventListener('submit', function (e) {
    e.preventDefault();
    if (busy) { busy.abort(); return; }
    var t = q.value.trim(); if (t) ask(t);
  });
  // Pages are swapped in place (hx-boost): the context line follows the page.
  document.addEventListener('htmx:afterSettle', function () { if (!pan.hidden && !busy) render(); });

  function setBusy(ctrl) {
    busy = ctrl; go.classList.toggle('stop', !!ctrl);
    go.setAttribute('aria-label', ctrl ? 'Stop' : 'Send');
    go.innerHTML = ctrl ? '<svg viewBox="0 0 24 24"><rect x="7" y="7" width="10" height="10" rx="1.5"/></svg>' : '<svg viewBox="0 0 24 24"><path d="M12 19V5M5 12l7-7 7 7"/></svg>';
  }

  function ask(text) {
    if (busy) return;
    q.value = ''; q.style.height = 'auto';
    log.push({ role: 'user', text: text });
    var a = { role: 'assistant', text: '', tools: [], live: true };
    log.push(a); render();
    var ctrl = new AbortController(); setBusy(ctrl);
    var finish = function (err) {
      a.live = false; if (err) a.error = err;
      setBusy(null); save(KEY_LOG, log.slice(-40)); render();
    };
    fetch('/assistant/chat', {
      method: 'POST', credentials: 'same-origin', signal: ctrl.signal,
      headers: { 'Content-Type': 'application/json', 'Accept': 'text/event-stream' },
      body: JSON.stringify({ message: text, conv: conv, page: location.pathname })
    }).then(function (res) {
      if (!res.ok || !res.body) {
        return res.json().then(function (j) { finish((j && j.error) || 'The assistant is not available right now.'); }, function () { finish('The assistant is not available right now.'); });
      }
      var reader = res.body.getReader(), dec = new TextDecoder(), buf = '';
      var pump = function () {
        return reader.read().then(function (r) {
          if (r.done) { finish(a.text || a.error ? null : 'The answer stopped early.'); return; }
          buf += dec.decode(r.value, { stream: true });
          var parts = buf.split('\n\n'); buf = parts.pop();
          parts.forEach(function (block) {
            var ev = 'message', data = '';
            block.split('\n').forEach(function (l) { if (l.indexOf('event:') === 0) ev = l.slice(6).trim(); else if (l.indexOf('data:') === 0) data += l.slice(5).trim(); });
            var d; try { d = JSON.parse(data || '{}'); } catch (e) { return; }
            if (ev === 'conv') { conv = d.id; save(KEY_CONV, conv); }
            else if (ev === 'tool') { a.tools.push({ id: d.id, label: d.label }); }
            else if (ev === 'tool_done') { a.tools.forEach(function (t) { if (t.id === d.id) { t.done = true; t.summary = d.summary; } }); }
            else if (ev === 'text') { a.text += d.t; }
            else if (ev === 'error') { a.error = d.message; }
          });
          render();
          return pump();
        });
      };
      return pump();
    }).catch(function (err) {
      finish(err && err.name === 'AbortError' ? (a.text ? null : 'Stopped.') : 'Couldn\'t reach the assistant. Check your connection and try again.');
    });
  }
  render();
})();
