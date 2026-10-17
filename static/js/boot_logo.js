// Loaded by templates/boot_logo.html.
function blTargets(){
  var v=document.querySelector('#bl-form input[name=target_type]:checked').value;
  document.getElementById('bl-t-devices').style.display = v==='devices'?'':'none';
  document.getElementById('bl-t-groups').style.display  = v==='groups'?'':'none';
}
function blFile(input){
  var f=input.files&&input.files[0];
  var pv=document.getElementById('bl-preview'), dt=document.getElementById('bl-dt'), th=document.getElementById('bl-th');
  if(!f){pv.style.display='none';dt.textContent='Drop a splash image, or click to choose';return;}
  var kb=(f.size/1024).toFixed(0)+' KB';
  document.getElementById('bl-pm').innerHTML='<b>'+f.name+'</b> · '+kb+' — wrapped into <span class="mono">splash.img</span> and hosted; visible next boot.';
  dt.textContent=f.name;
  if(/^image\/(png|jpeg|bmp)/.test(f.type)){
    var rd=new FileReader();
    rd.onload=function(e){th.innerHTML='<img src="'+e.target.result+'" alt="preview">';};
    rd.readAsDataURL(f);
  } else { th.textContent='img'; }
  pv.style.display='';
}

// The zone was labelled "Drop a splash image" but nothing listened for a drop —
// only the click opened the picker. Wire the drop for real, and hand the file to
// the same input so validation and submit are unchanged.
(function(){
  var zone=document.getElementById('bl-drop'), input=document.getElementById('bl-file');
  if(!zone||!input) return;
  ['dragenter','dragover'].forEach(function(ev){
    zone.addEventListener(ev,function(e){e.preventDefault();e.stopPropagation();zone.classList.add('over');});
  });
  ['dragleave','drop'].forEach(function(ev){
    zone.addEventListener(ev,function(e){e.preventDefault();e.stopPropagation();zone.classList.remove('over');});
  });
  zone.addEventListener('drop',function(e){
    var files=e.dataTransfer&&e.dataTransfer.files;
    if(!files||!files.length) return;
    // DataTransfer is assignable to a file input in every browser we target; it
    // keeps one code path for picked and dropped files.
    try{ input.files=files; }catch(err){ return; }
    blFile(input);
  });
})();
