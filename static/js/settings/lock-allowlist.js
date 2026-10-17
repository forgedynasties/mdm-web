// Loaded by templates/settings.html.
function kioskFilter(q){
  q=(q||'').trim().toLowerCase(); var n=0;
  document.querySelectorAll('#kioskfleet .kd-app').forEach(function(b){ var hit=!q || b.textContent.toLowerCase().indexOf(q)!==-1; b.hidden=!hit; if(hit)n++; });
  var none=document.getElementById('kd-none'); if(!none){ none=document.createElement('div'); none.id='kd-none'; none.className='kd-none'; document.getElementById('kioskfleet').appendChild(none); }
  none.textContent='No app matches "'+q+'"'; none.hidden=n>0||!q;
}
function kioskAllowed(pkg){
  var ta=document.getElementById('kiosk-allowlist-ta'); if(!ta) return false;
  return ta.value.split(/[\s,;]+/).some(function(pat){ pat=pat.trim(); if(!pat) return false; if(pat===pkg) return true;
    if(pat.indexOf('*')===-1) return false; var re=new RegExp('^'+pat.split('*').map(function(x){return x.replace(/[.+?^${}()|[\]\\]/g,'\\$&');}).join('.*')+'$'); return re.test(pkg); });
}
function kioskSyncAdded(){ document.querySelectorAll('#kioskfleet .kd-app').forEach(function(b){ b.classList.toggle('added', kioskAllowed(b.dataset.pkg)); }); }
function kioskAddPkg(pkg, el){
  var ta=document.getElementById('kiosk-allowlist-ta'); if(!ta) return;
  if(kioskAllowed(pkg)){ // toggle off: remove the exact line (patterns stay)
    ta.value=ta.value.split(/\n/).filter(function(l){return l.trim()!==pkg;}).join('\n'); kioskSyncAdded(); return;
  }
  var lines=ta.value.split(/[\s,;]+/).map(function(s){return s.trim();}).filter(Boolean);
  lines.push(pkg); ta.value=lines.join('\n'); kioskSyncAdded();
}
(function(){ var ta=document.getElementById('kiosk-allowlist-ta'); if(ta){ ta.addEventListener('input', kioskSyncAdded); kioskSyncAdded(); } })();
