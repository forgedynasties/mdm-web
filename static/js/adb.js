// Loaded by templates/adb.html.
function adbBoxes(){return Array.prototype.filter.call(document.querySelectorAll('#adb-batch td input[name=serial]:not(:disabled)'),function(c){return !c.closest('tr').classList.contains('lt-out')})}
function adbCount(){var n=document.querySelectorAll('#adb-batch td input[name=serial]:checked:not(:disabled)').length;document.getElementById('adb-count').textContent=n;document.getElementById('adb-bar').hidden=n===0;}
function adbClear(){adbBoxes().forEach(function(c){c.checked=false});document.getElementById('adb-all').checked=false;adbCount();}
document.querySelectorAll('#adb-batch td input[name=serial]:disabled').forEach(function(c){c.checked=false});
adbBoxes().forEach(function(c){c.addEventListener('change',adbCount)});
document.getElementById('adb-all').addEventListener('change',function(){var on=this.checked;adbBoxes().forEach(function(c){c.checked=on});adbCount();});
adbCount();
// A row the search or a chip hides is not part of the batch: untick it.
document.getElementById('adb-list').addEventListener('lt:change',function(){document.querySelectorAll('#adb-batch tr.lt-out input[name=serial]:checked').forEach(function(c){c.checked=false});document.getElementById('adb-all').checked=false;adbCount();});
// Remember every address this browser has allowed, so the next tunnel is prefilled with
// all of them: an office with two WAN links sends adb out through an address the server
// only learns when it refuses it, and that should happen once, not every session.
(function(){
  var KEY='adb-allow', mem=[];
  try{mem=JSON.parse(localStorage.getItem(KEY)||'[]')}catch(e){}
  function remember(list){list.split(/[,\s]+/).forEach(function(a){if(a&&mem.indexOf(a)<0)mem.push(a)});try{localStorage.setItem(KEY,JSON.stringify(mem))}catch(e){}}
  var live=document.getElementById('adb-allowed'); if(live) remember(live.textContent);
  var box=document.getElementById('adb-allow');
  if(box){
    var all=[box.dataset.mine].concat(mem.filter(function(a){return a!==box.dataset.mine}));
    box.value=all.join(', ');
    box.form.addEventListener('submit',function(){remember(box.value)});
  }
})();
if (window.ADB_PAGE.sessions) {
// A live tunnel's state changes under the page (attempt, refused, connected): refresh
// while nothing is ticked, so the steps track `adb connect` without a hand reload.
setInterval(function(){if(!document.querySelector('#adb-batch td input[name=serial]:checked:not(:disabled)')&&document.visibilityState==='visible')location.reload()},15000);
}
