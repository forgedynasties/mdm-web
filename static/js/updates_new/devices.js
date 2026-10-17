// Loaded by templates/updates_new.html.
// The device list is the shared picker (static/picker.js); the count and the
// push button follow its basket.
function pushCount(){
  var f=document.getElementById('pk-serials');
  var n=f&&f.value?f.value.split('\n').filter(Boolean).length:0;
  var hasRel=(((document.querySelector('#push-form input[name="release_id"]'))||{}).value||'')!=='';
  var lbl=document.getElementById('push-lbl');
  if(lbl)lbl.textContent='Push to '+n+' device'+(n===1?'':'s');
  var btn=document.getElementById('push-btn');
  if(btn)btn.disabled=!hasRel||n===0;
}
document.addEventListener('pk:change', pushCount);
document.addEventListener('DOMContentLoaded', pushCount);
function pushConfirm(e){
  var f=document.getElementById('pk-serials'); var n=f&&f.value?f.value.split('\n').filter(Boolean).length:0;
  if(n===0){ e.preventDefault(); return false; }
  var w=document.getElementById('push-allow-wipe');
  if(w&&w.checked){
    return mdmConfirmSubmit(e,{title:'Push update and allow factory reset?',body:'This pushes the update to '+n+' selected device'+(n===1?'':'s')+'. Any of them without an incremental for its current build gets the full image, which WIPES the device: all apps, settings and data are erased.',confirmLabel:'Push and allow reset'});
  }
  return mdmConfirmSubmit(e,{title:'Push update?',body:'This pushes the update to '+n+' selected device'+(n===1?'':'s')+'.',confirmLabel:'Push update'});
}
function pushReboot(v,btn){document.getElementById('push-reboot').value=v;btn.parentElement.querySelectorAll('.seg-btn').forEach(function(s){s.classList.toggle('active',s===btn);});}
function pushDelivery(v,btn){var el=document.getElementById('push-delivery');if(el)el.value=v;btn.parentElement.querySelectorAll('.seg-btn').forEach(function(s){s.classList.toggle('active',s===btn);});}
