// Loaded by templates/settings.html.
// ── App library: upload APK (client-parsed, direct-to-S3) with a progress bar ──
function libEnsureParser(){return new Promise(function(res,rej){if(window.AppInfoParser)return res();var s=document.createElement('script');s.src='/static/vendor/app-info-parser.min.js';s.onload=function(){res();};s.onerror=function(){rej(new Error('could not load APK parser'));};document.head.appendChild(s);});}
// 192px, not 96: the store renders icons on 136px tiles and at 96 they visibly upscaled.
// Capped at the source's own size so a small icon is not blown up on the way in either.
function libIconPng(ic){return new Promise(function(res){if(!ic)return res('');var src=ic.indexOf('data:')===0?ic:('data:image/png;base64,'+ic);var img=new Image();img.onload=function(){try{var n=Math.min(192,Math.max(img.naturalWidth||192,96));var c=document.createElement('canvas');c.width=n;c.height=n;var g=c.getContext('2d');g.imageSmoothingQuality='high';g.drawImage(img,0,0,n,n);res(c.toDataURL('image/png').split(',')[1]||'');}catch(e){res('');}};img.onerror=function(){res('');};img.src=src;});}
function libFmt(n){if(n<1024)return n+' B';if(n<1048576)return (n/1024).toFixed(0)+' KB';if(n<1073741824)return (n/1048576).toFixed(1)+' MB';return (n/1073741824).toFixed(2)+' GB';}
function libS3Put(url,file,onp,onXhr){return new Promise(function(res,rej){var x=new XMLHttpRequest();if(onXhr)onXhr(x);x.open('PUT',url);x.setRequestHeader('Content-Type','application/vnd.android.package-archive');x.upload.onprogress=function(e){if(e.lengthComputable&&onp)onp(e.loaded,e.total);};x.onload=function(){(x.status>=200&&x.status<300)?res():rej(new Error('S3 upload failed ('+x.status+')'));};x.onerror=function(){rej(new Error('network error uploading to S3'));};x.onabort=function(){rej(new Error('cancelled'));};x.send(file);});}
// Multi-file APK upload: each file parses + uploads to S3 with its own progress and a
// cancel button; once uploads finish, a confirmation window lists the parsed details and
// only then (on confirm) registers them into the library.
var libQueue = [];
// Packages already in the library (from the tiles), for the duplicate hint.
function libExisting(){var m={};document.querySelectorAll('.lib-tile[data-pkg]').forEach(function(t){m[t.dataset.pkg]=t.dataset.version||'';});return m;}
function libAddOpen(){document.getElementById('lib-add-modal').classList.add('modal-open');libSum();}
function libAddClose(){
  if(libQueue.some(function(it){return it.state==='reading'||it.state==='uploading';})&&!confirm('Uploads are still running. Close and cancel them?'))return;
  libQueue.forEach(function(it){if(it.state==='reading'||it.state==='uploading'){it.state='cancelled';if(it.xhr)it.xhr.abort();}});
  libQueue=[]; document.getElementById('la-list').innerHTML=''; document.getElementById('la-err').style.display='none';
  document.getElementById('lib-add-modal').classList.remove('modal-open');
}
function libUploadApks(files){
  if(!files||!files.length)return;
  var apks=Array.prototype.filter.call(files,function(f){return /\.apk$/i.test(f.name)||f.type==='application/vnd.android.package-archive';});
  var skipped=files.length-apks.length;
  libAddOpen();
  if(skipped){var e=document.getElementById('la-err');e.textContent=skipped+' file'+(skipped===1?' is':'s are')+' not an APK and '+(skipped===1?'was':'were')+' ignored.';e.style.display='block';}
  apks.forEach(function(file){
    var item={id:'ul_'+libQueue.length+'_'+(+new Date()),file:file,name:file.name,pkg:'',ver:'',icon:'',size:file.size,key:'',xhr:null,state:'reading',err:''};
    libQueue.push(item); document.getElementById('la-list').appendChild(libUlRow(item)); libProcess(item);
  });
  libSum();
}
function libUlRow(item){
  var row=document.createElement('div'); row.className='la-row'; row.id=item.id;
  row.innerHTML='<div class="la-ic"></div><div class="la-m"><div class="la-nm"></div><div class="la-pk mono"></div><div class="la-bar"><div class="la-fill"></div></div><div class="la-st"></div></div><div class="la-meta"></div><button type="button" class="la-x" title="Remove">&times;</button>';
  row.querySelector('.la-nm').textContent=item.name;
  row.querySelector('.la-meta').textContent=libFmt(item.size);
  row.querySelector('.la-x').onclick=function(){libCancel(item);};
  return row;
}
function libUlSet(item,txt,pct,err){
  var row=document.getElementById(item.id); if(!row)return;
  var st=row.querySelector('.la-st'); if(txt!=null)st.textContent=txt; st.classList.toggle('is-error',!!err); st.classList.toggle('is-ok',txt==='Ready');
  if(pct!=null)row.querySelector('.la-fill').style.width=pct+'%';
  row.querySelector('.la-bar').hidden=(pct==null||pct>=100||!!err);
  libSum();
}
function libSum(){
  var ready=libQueue.filter(function(it){return it.state==='uploaded';}).length, busy=libQueue.some(function(it){return it.state==='reading'||it.state==='uploading';});
  var btn=document.getElementById('la-add'), sum=document.getElementById('la-sum'); if(!btn)return;
  btn.disabled=busy||!ready; btn.textContent=ready?('Add '+ready+' to library'):'Add to library';
  sum.textContent=!libQueue.length?'':busy?'Uploading…':(ready+' ready');
}
function libProcess(item){
  libUlSet(item,'Reading…',0);
  var r=document.getElementById(item.id);
  libEnsureParser().then(function(){return new AppInfoParser(item.file).parse();}).then(function(info){
    item.pkg=info.package||'';
    var lbl=(info.application&&info.application.label)||info.label||item.pkg; if(Array.isArray(lbl))lbl=lbl[0]; item.name=String(lbl||item.pkg);
    var v=info.versionName||(info.apkInfo&&info.apkInfo.versionName)||''; item.ver=v?String(v):'';
    if(r){ r.querySelector('.la-nm').textContent=item.name; r.querySelector('.la-pk').textContent=item.pkg; r.querySelector('.la-meta').textContent=(item.ver?'v'+item.ver+' · ':'')+libFmt(item.size);
      var have=libExisting(); if(item.pkg&&item.pkg in have){var tag=document.createElement('span');tag.className='la-dup';tag.textContent=have[item.pkg]===item.ver?'already in library':'in library as v'+(have[item.pkg]||'?');r.querySelector('.la-nm').appendChild(tag);} }
    return libIconPng(info.icon).then(function(png){item.icon=png; var ic=r&&r.querySelector('.la-ic'); if(ic&&png)ic.innerHTML='<img src="data:image/png;base64,'+png+'" alt="">';});
  }).catch(function(){ if(r){r.querySelector('.la-pk').textContent='could not read the manifest; the file still uploads';} })
  .then(function(){ if(item.state==='cancelled')throw new Error('cancelled'); return fetch('/apps/upload-url',{method:'POST',headers:{'Content-Type':'application/json'},credentials:'same-origin',body:JSON.stringify({filename:item.file.name})}); })
  .then(function(r){if(!r.ok)throw new Error('could not get upload URL');return r.json();})
  .then(function(d){ item.key=d.key; item.state='uploading'; libUlSet(item,'Uploading… 0%',0);
    return libS3Put(d.url,item.file,function(l,t){var p=Math.round(l/t*100);libUlSet(item,'Uploading… '+p+'%',p);},function(x){item.xhr=x;}); })
  .then(function(){ item.state='uploaded'; item.xhr=null; libUlSet(item,'Ready',100); })
  .catch(function(err){ if(item.state==='cancelled'){libUlSet(item,'Cancelled',null,true);} else {item.state='error'; item.err=err.message; libUlSet(item,'Error: '+err.message,null,true);} });
}
function libCancel(item){
  if(item.state==='reading'||item.state==='uploading'){item.state='cancelled'; if(item.xhr)item.xhr.abort();}
  var r=document.getElementById(item.id); if(r)r.remove(); var i=libQueue.indexOf(item); if(i>=0)libQueue.splice(i,1); libSum();
}
function libConfirmAdd(){
  var items=libQueue.filter(function(it){return it.state==='uploaded';}); if(!items.length)return;
  var btn=document.getElementById('la-add'); btn.disabled=true; btn.textContent='Adding…';
  var err=document.getElementById('la-err'); err.style.display='none'; var added=0; var chain=Promise.resolve();
  items.forEach(function(it){ chain=chain.then(function(){return fetch('/apps/register',{method:'POST',headers:{'Content-Type':'application/json'},credentials:'same-origin',body:JSON.stringify({key:it.key,package:it.pkg,version:it.ver,name:it.name,icon:it.icon})}).then(function(r){if(!r.ok)return r.text().then(function(t){throw new Error(t||('HTTP '+r.status));});added++;});}); });
  chain.then(function(){location.reload();}).catch(function(e){btn.disabled=false;btn.textContent='Add '+items.length+' to library';err.textContent='Added '+added+' of '+items.length+'. '+e.message;err.style.display='block';});
}
function libDelete(form,ev){ev.preventDefault();if(!confirm('Remove this APK from the library? Devices that already have it keep it.'))return false;fetch(form.action,{method:'POST',credentials:'same-origin',headers:{'X-Requested-With':'fetch'}}).then(function(){location.reload();}).catch(function(){location.reload();});return false;}
function libFamOpen(id){var m=document.getElementById('lf-'+id);if(m)m.classList.add('modal-open');}
function libFamClose(m){if(m)m.classList.remove('modal-open');}
// Drag and drop: anywhere on the page. Capture phase on document so no region's own
// handlers can swallow it; files taken from dataTransfer.files or .items (some
// browsers only fill one of them for an OS drag).
(function(){
  if(window.__libDnD){return;} window.__libDnD=true;
  function hasFiles(e){var dt=e.dataTransfer; if(!dt)return false; var t=dt.types; if(t){ if(Array.prototype.some.call(t,function(x){return x==='Files'||x==='application/x-moz-file';}))return true; } if(dt.items){for(var i=0;i<dt.items.length;i++){if(dt.items[i].kind==='file')return true;}} return !!(dt.files&&dt.files.length);}
  function filesOf(e){var dt=e.dataTransfer, out=[]; if(dt.files&&dt.files.length){for(var i=0;i<dt.files.length;i++)out.push(dt.files[i]);} else if(dt.items){for(var j=0;j<dt.items.length;j++){var f=dt.items[j].kind==='file'?dt.items[j].getAsFile():null; if(f)out.push(f);}} return out;}
  var depth=0;
  function card(){return document.getElementById('lib-card');}
  function zone(){return document.getElementById('la-drop');}
  document.addEventListener('dragenter',function(e){ if(!hasFiles(e))return; e.preventDefault(); depth++; document.body.classList.add('lib-dragging'); var c=card(); if(c&&c.contains(e.target))c.classList.add('is-over'); var z=zone(); if(z&&z.contains(e.target))z.classList.add('is-over'); },true);
  document.addEventListener('dragover',function(e){ if(!hasFiles(e))return; e.preventDefault(); if(e.dataTransfer)e.dataTransfer.dropEffect='copy'; },true);
  document.addEventListener('dragleave',function(e){ if(!hasFiles(e))return; depth=Math.max(0,depth-1); if(!depth){document.body.classList.remove('lib-dragging'); var c=card(); if(c)c.classList.remove('is-over'); var z=zone(); if(z)z.classList.remove('is-over');} },true);
  document.addEventListener('drop',function(e){ if(!hasFiles(e)){ try{console.warn('[mdm] drop ignored: no files in dataTransfer', e.dataTransfer&&e.dataTransfer.types);}catch(x){} return; } e.preventDefault(); e.stopPropagation(); depth=0; document.body.classList.remove('lib-dragging'); var c=card(); if(c)c.classList.remove('is-over'); var z=zone(); if(z)z.classList.remove('is-over'); var files=filesOf(e); try{console.info('[mdm] drop:', files.map(function(f){return f.name;}));}catch(x){} if(!files.length){ if(window.showToast)showToast('Nothing droppable in that drag', 'error'); return; } if(typeof libUploadApks==='function')libUploadApks(files); },true);
})();
