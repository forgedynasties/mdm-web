// Loaded by templates/device.html.
(function () {
  var SERIAL = window.DEVICE_PAGE.serial;
  var cache = {};
  function el(id) { return document.getElementById(id); }
  function fmtDate(s) { try { return new Date(s).toLocaleString(); } catch (e) { return s; } }

  window.aiOpen = function () { el('ai-modal').classList.add('modal-open'); aiLoadPrev(); };
  window.aiClose = function () { el('ai-modal').classList.remove('modal-open'); };
  window.aiBackdrop = function (e) { if (e.target.id === 'ai-modal') aiClose(); };

  function aiLoadPrev() {
    var box = el('ai-prev'); box.innerHTML = '<div class="muted small">Loading…</div>';
    fetch('/devices/' + encodeURIComponent(SERIAL) + '/ai-analyses')
      .then(function (r) { return r.json(); })
      .then(function (j) {
        var list = (j && j.analyses) || []; cache = {};
        if (!list.length) { box.innerHTML = '<div class="muted small">No previous analyses yet.</div>'; return; }
        box.innerHTML = list.map(function (a) {
          cache[a.id] = a;
          var snip = (a.text || '').replace(/[#*`>_]/g, '').replace(/\s+/g, ' ').trim().slice(0, 80);
          return '<button type="button" class="ai-prev-row" onclick="aiShowPrev(' + a.id + ')"><span class="ai-prev-snip">' + snip + '…</span><span class="muted small">' + fmtDate(a.created_at) + '</span></button>';
        }).join('');
      })
      .catch(function () { box.innerHTML = '<div class="muted small">Couldn\'t load history.</div>'; });
  }

  window.aiShowPrev = function (id) {
    var a = cache[id]; if (!a) return;
    el('ai-result').innerHTML = window.mdmRenderMarkdown(a.text);
    el('ai-meta').textContent = a.model + ' · ' + fmtDate(a.created_at);
  };

  window.aiRun = function () {
    var btn = el('ai-run-btn'), out = el('ai-result'), meta = el('ai-meta');
    btn.disabled = true; var label = btn.innerHTML; btn.innerHTML = 'Analyzing…';
    out.innerHTML = '<span class="muted">Reading 30 days of telemetry…</span>'; meta.textContent = '';
    fetch('/devices/' + encodeURIComponent(SERIAL) + '/ai-analysis', { method: 'POST' })
      .then(function (r) { return r.json().then(function (j) { return { ok: r.ok, j: j }; }); })
      .then(function (res) {
        if (!res.ok) { out.innerHTML = '<span class="muted">' + (res.j.error || 'Analysis failed.') + '</span>'; return; }
        out.innerHTML = window.mdmRenderMarkdown(res.j.text);
        meta.textContent = res.j.model + ' · ' + fmtDate(res.j.generated_at);
        aiLoadPrev();
      })
      .catch(function () { out.innerHTML = '<span class="muted">Network error.</span>'; })
      .finally(function () { btn.disabled = false; btn.innerHTML = label; });
  };
})();
