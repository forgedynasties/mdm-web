// Loaded by templates/users.html.
document.addEventListener('keydown', function (e) {
  if (e.key === 'Escape') { var m = document.getElementById('add-user-modal'); if (m) m.classList.remove('modal-open'); }
});

(function(){
  var q=document.getElementById('uf-q'), role=document.getElementById('uf-role'), acc=document.getElementById('uf-access'), st=document.getElementById('uf-status');
  if(!q) return;
  var rows=Array.prototype.slice.call(document.querySelectorAll('.uu-row[data-q]'));
  function apply(){
    var t=q.value.trim().toLowerCase(), r=role.value, a=acc.value, s=st.value, n=0, now=Date.now();
    rows.forEach(function(row){
      var d=row.dataset, ok=true;
      if(t && d.q.toLowerCase().indexOf(t)===-1) ok=false;
      if(ok && r && d.role!==r) ok=false;
      if(ok && a==='custom' && d.access!=='custom') ok=false;
      if(ok && a==='default' && d.access!=='default') ok=false;
      if(ok && a==='sessions' && d.sessions!=='1') ok=false;
      if(ok && s){
        var last=d.last?Date.parse(d.last):NaN, days=isNaN(last)?Infinity:(now-last)/864e5;
        if(s==='active7' && !(days<=7)) ok=false;
        if(s==='idle30' && !(days>30 && isFinite(days))) ok=false;
        if(s==='never' && isFinite(days)) ok=false;
        if(s==='unverified' && d.verified!=='0') ok=false;
      }
      row.hidden=!ok; if(ok)n++;
    });
    var any=!!(t||r||a||s);
    document.getElementById('uf-count').textContent=any? n+' of '+rows.length : '';
    document.getElementById('uf-clear').hidden=!any;
    document.getElementById('uf-none').hidden=n>0;
    try{ sessionStorage.setItem('uf', JSON.stringify({q:q.value,r:r,a:a,s:s})); }catch(e){}
  }
  window.ufClear=function(){ q.value=''; role.value=''; acc.value=''; st.value=''; apply(); };
  [q,role,acc,st].forEach(function(el){ el.addEventListener('input',apply); el.addEventListener('change',apply); });
  try{ var saved=JSON.parse(sessionStorage.getItem('uf')||'null'); if(saved){ q.value=saved.q||''; role.value=saved.r||''; acc.value=saved.a||''; st.value=saved.s||''; } }catch(e){}
  apply();
})();
