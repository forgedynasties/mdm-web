// Loaded by templates/alerts.html. Search, views, chips and sort are coll_list.js (L1, 3 Nov 2026).
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
      if(fresh&&cur){
        // The list (coll_list.js) keeps its view, chips, search and sort across the swap.
        var old=cur.querySelector('[data-cl]'), keep=old&&old._clState;
        cur.innerHTML=fresh.innerHTML;
        var nr=cur.querySelector('[data-cl]'); if(nr&&keep) nr._clCarry=keep;
        if(window.collListBoot) window.collListBoot();
      }
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

// The left list switches between the problem list and the Crashes pane in place: nothing
// but the pane changes (3 Nov 2026). The address follows, so a reload lands on the same pane.
document.addEventListener('click',function(e){
  var c=e.target.closest('[data-al-crashes]'), f=!c&&e.target.closest('#alerts-live [data-clf]');
  if(!c&&!f) return;
  var list=document.getElementById('al-list'), cr=document.getElementById('al-crash'); if(!list||!cr) return;
  var dev=new URLSearchParams(location.search).get('device'), u=new URL(location.href);
  u.search=''; if(dev) u.searchParams.set('device',dev);
  if(c){
    list.hidden=true; cr.hidden=false;
    document.querySelectorAll('#alerts-live [data-clf]').forEach(function(x){ x.classList.remove('on'); x.setAttribute('aria-pressed','false'); });
    c.classList.add('on'); c.setAttribute('aria-pressed','true');
    u.searchParams.set('view','crashes');
  } else {
    list.hidden=false; cr.hidden=true;
    var cb=document.querySelector('[data-al-crashes]'); if(cb){ cb.classList.remove('on'); cb.setAttribute('aria-pressed','false'); }
    u.searchParams.set('show',f.dataset.clf||'open');
  }
  history.replaceState(history.state,'',u.pathname+u.search);
});
