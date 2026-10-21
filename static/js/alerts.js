// Loaded by templates/alerts.html.
// Client-side search: hide cards whose text doesn't match, preserved across the
// live refresh below.
window.__alxQ = window.__alxQ || "";
// A1 filter pills (Severity, Owner, Venue): {sev, owner, venue}, matched against the
// problem rows' data-sev / data-owner / data-venue. Kept across the live refresh.
window.__alxF = window.__alxF || {};
function alxPill(sel){
  var pill=sel.closest('[data-f]'); if(!pill) return;
  window.__alxF[pill.getAttribute('data-f')]=sel.value;
  alxFilter();
}
function alxFilter(){
  var inp=document.getElementById('alx-q'); if(inp) window.__alxQ=inp.value;
  var q=(window.__alxQ||"").trim().toLowerCase(), f=window.__alxF||{}, shown=0;
  document.querySelectorAll('#alerts-live [data-f]').forEach(function(p){
    var k=p.getAttribute('data-f'), s=p.querySelector('select'); if(s) s.value=f[k]||"";
    p.classList.toggle('set', !!f[k]);
  });
  var pills=!!(f.sev||f.owner||f.venue), on=!!q||pills;
  var cards=document.querySelectorAll('#alerts-live .alx-al, #alerts-live tr[data-search]');
  cards.forEach(function(c){
    var hay=(c.getAttribute('data-search')||"").toLowerCase();
    var ok=!q||hay.indexOf(q)!==-1;
    if(ok && c.tagName==='TR'){
      if(f.sev && c.getAttribute('data-sev')!==f.sev) ok=false;
      if(f.owner && c.getAttribute('data-owner')!==f.owner) ok=false;
      if(f.venue && c.getAttribute('data-venue')!==f.venue) ok=false;
    }
    c.style.display=ok?"":"none"; if(ok)shown++;
  });
  document.querySelectorAll('#alerts-live .alx-sec, #alerts-live .alx-fold, #alerts-live .alx-pager').forEach(function(s){ s.style.display=on?"none":""; });
  // A search that matches a device folded inside a group must not leave it hidden
  // behind a collapsed expander — open every group while filtering.
  document.querySelectorAll('#alerts-live .alx-fold-g').forEach(function(d){ d.open=!!q; });
  var nm=document.getElementById('alx-nomatch'); if(nm) nm.style.display=(on&&cards.length&&shown===0)?"block":"none";
}
// Live refresh: re-fetch the current (filter-preserving) URL and swap the region.
(function(){
  var pending=false;
  function refresh(){
    // An open menu may hold a half-typed note: wait until it closes.
    if(document.querySelector('#alerts-live .alx-menu[open]')){ window.__alxStale=true; return; }
    if(pending) return; pending=true;
    fetch(location.href,{credentials:'same-origin'}).then(function(r){return r.text();}).then(function(html){
      pending=false;
      var doc=new DOMParser().parseFromString(html,'text/html');
      var fresh=doc.getElementById('alerts-live'), cur=document.getElementById('alerts-live');
      if(fresh&&cur){ cur.innerHTML=fresh.innerHTML; var inp=document.getElementById('alx-q'); if(inp)inp.value=window.__alxQ||""; alxFilter(); }
    }).catch(function(){pending=false;});
  }
  document.addEventListener('mdm:alerts-update',refresh);
  // Picking an option submits the form (boosted) but left the <details> open, and an
  // open menu holds off the refresh — so the row only changed after a click elsewhere.
  // Close the menu on submit and refresh once the request has landed.
  document.addEventListener('submit',function(e){ var m=e.target.closest&&e.target.closest('#alerts-live .alx-menu'); if(m) m.open=false; },true);
  document.body.addEventListener('htmx:afterRequest',function(e){ var el=e.detail&&e.detail.elt; if(el&&el.closest&&el.closest('#alerts-live .alx-pop')){ window.__alxStale=false; refresh(); } });
  // One problem menu open at a time; a click elsewhere closes it.
  document.addEventListener('click',function(e){
    document.querySelectorAll('#alerts-live .alx-menu[open]').forEach(function(m){ if(!m.contains(e.target)) m.open=false; });
    if(window.__alxStale && !document.querySelector('#alerts-live .alx-menu[open]')){ window.__alxStale=false; refresh(); }
  });
  document.addEventListener('keydown',function(e){
    if(e.key!=='Escape') return;
    document.querySelectorAll('#alerts-live .alx-menu[open]').forEach(function(m){ m.open=false; var s=m.querySelector('summary'); if(s) s.focus(); });
  });
})();
