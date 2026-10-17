// Loaded by templates/access_profiles.html.
(function(){
  var f = document.getElementById('ap-form'), editing = '';
  function $(id){ return document.getElementById(id); }
  window.apOpen = function(){ editing = ''; f.reset(); $('ap-title').textContent = 'New profile'; $('ap-err').textContent = ''; f.hidden = false; f.scrollIntoView({behavior:'smooth', block:'start'}); $('ap-name').focus(); };
  window.apEdit = function(btn){
    var row = btn.closest('.ap-row'); apOpen(); editing = row.dataset.id;
    $('ap-title').textContent = 'Edit ' + row.dataset.name;
    $('ap-name').value = row.dataset.name; $('ap-desc').value = row.dataset.desc;
    var has = row.dataset.actions.split(',');
    f.querySelectorAll('input[name=actions]').forEach(function(c){ if (!c.disabled) c.checked = has.indexOf(c.value) >= 0; });
  };
  window.apSave = function(e){
    e.preventDefault(); $('ap-save').disabled = true;
    var enc = new URLSearchParams(); new FormData(f).forEach(function(v, k){ enc.append(k, v); });
    fetch('/users/access/profiles' + (editing ? '/' + editing : ''), { method:'POST', credentials:'same-origin', headers:{'HX-Request':'true','Content-Type':'application/x-www-form-urlencoded'}, body: enc }).then(function(r){
      $('ap-save').disabled = false;
      if (r.ok) { location.href = '/users/access/profiles?flash=' + encodeURIComponent('Profile saved') + '&flash_type=ok'; return; }
      return r.text().then(function(t){ $('ap-err').textContent = t.trim(); });
    });
    return false;
  };
})();
