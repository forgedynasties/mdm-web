// Loaded by templates/owner_home.html (TM1): the invite and remove dialogs on the Your team card.
(function () {
  var inv = document.getElementById('tm1-inv'), rmd = document.getElementById('tm1-rmd');
  if (!inv || !rmd || !inv.showModal) return;
  var open = document.getElementById('tm1-open');
  var err = document.getElementById('tm1-err');
  function close(d) { if (d.open) d.close(); }

  open.addEventListener('click', function () {
    err.hidden = true;
    inv.showModal();
    document.getElementById('tm1-email').focus();
  });
  [inv, rmd].forEach(function (d) {
    d.addEventListener('click', function (e) {
      if (e.target === d || (e.target.closest && e.target.closest('[data-close]'))) close(d);
    });
  });

  // Invite: sent in the background so a refusal (no email, a team address, no venue picked)
  // reads in the dialog; on success the server's redirect to /?team=invited is followed.
  document.getElementById('tm1-invf').addEventListener('submit', function (e) {
    var f = e.target;
    if (!window.fetch || !window.FormData || !window.URLSearchParams) return;
    e.preventDefault();
    var btn = f.querySelector('button[type=submit]');
    btn.disabled = true;
    fetch(f.action, { method: 'POST', credentials: 'same-origin', body: new URLSearchParams(new FormData(f)) })
      .then(function (r) {
        if (r.ok && r.redirected) { window.location.href = r.url; return; }
        return r.text().then(function (t) {
          err.textContent = (t || 'Could not send the invite.').trim();
          err.hidden = false;
          btn.disabled = false;
        });
      })
      .catch(function () { err.textContent = 'Could not send the invite. Check your connection.'; err.hidden = false; btn.disabled = false; });
  });

  // Remove: our own confirm in place of the browser's.
  [].forEach.call(document.querySelectorAll('.tm1-rm'), function (b) {
    b.addEventListener('click', function () {
      document.getElementById('tm1-rmf').setAttribute('action', b.getAttribute('data-action'));
      document.getElementById('tm1-rm-n').textContent = b.getAttribute('data-name');
      rmd.showModal();
    });
  });
})();
