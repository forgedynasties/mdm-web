// Loaded by templates/settings.html.
(function(){
  var root = document.getElementById('google-usage');
  if(!root) return;
  var nf = new Intl.NumberFormat();
  function money(v){ return '$' + (Number(v)||0).toFixed(2); }
  function setText(el, sel, val){ var n=el.querySelector(sel); if(n) n.textContent=val; }
  function renderCard(s){
    var el = document.getElementById('gu-card-'+s.key);
    if(!el) return;
    el.classList.toggle('off', !s.enabled);
    setText(el, '.gu-requests', nf.format(s.requests));
    setText(el, '.gu-24', nf.format(s.last24h));
    setText(el, '.gu-peak', nf.format(s.peak_hour));
    setText(el, '.gu-hits', nf.format(s.hits));
    setText(el, '.gu-localhits', nf.format(s.local_hits||0));
    setText(el, '.gu-cooldowns', nf.format(s.cooldowns));
    setText(el, '.gu-errors', nf.format(s.errors));
    setText(el, '.gu-hitratio', s.hit_ratio + '%');
    if(s.billable){
      setText(el, '.gu-cost-val', money(s.cost_to_date));
      setText(el, '.gu-proj-val', money(s.proj_month));
    }
    setText(el, '.gu-lastreq', s.last_req_ago || 'never');
    var errline = el.querySelector('.gu-errline');
    if(errline){ errline.innerHTML = s.last_err ? (' · <span class="gu-err" title="'+(s.last_err.replace(/"/g,'&quot;'))+'">error '+s.last_err_ago+'</span>') : ''; }
    // sparkline
    var spark = el.querySelector('.gu-spark'); var bars = spark ? spark.querySelectorAll('.gu-bar') : [];
    var peak = s.peak_hour || 0;
    (s.hourly||[]).forEach(function(v,i){
      var b = bars[i]; if(!b) return;
      var h = (peak>0 && v>0) ? Math.max(6, Math.round(v*100/peak)) : 0;
      b.style.height = h + '%';
      b.classList.toggle('zero', v===0);
    });
  }
  function render(data){
    (data.stats||[]).forEach(renderCard);
    setText(root, '#gu-total-cost', money(data.total_cost_to_date));
    setText(root, '#gu-total-proj', money(data.total_proj_month));
    var L = data.learned || {};
    if(document.getElementById('gu-l-total')){
      setText(document, '#gu-l-total', nf.format(L.total||0));
      setText(document, '#gu-l-fresh', nf.format(L.fresh||0));
      setText(document, '#gu-l-places', nf.format(L.distinct_places||0));
      setText(document, '#gu-l-served', nf.format(L.served_lookups||0));
      setText(document, '#gu-l-saved', money(L.cost_saved));
    }
    setText(root, '#gu-since', data.since_ago || '');
    var u = document.getElementById('gu-updated'); if(u) u.textContent='updated just now';
  }
  try { render(window.SETTINGS_GOOGLE_USAGE); } catch(e){}
  var url = root.getAttribute('data-poll');
  function poll(){
    fetch(url, {headers:{'Accept':'application/json'}, credentials:'same-origin'})
      .then(function(r){ return r.ok ? r.json() : null; })
      .then(function(d){ if(d) render(d); })
      .catch(function(){});
  }
  // Poll only while this tab is visible and the page is focused.
  setInterval(function(){
    var tab = document.querySelector('.settings-tab[data-tab="integrations"]');
    if(tab && tab.classList.contains('active') && !document.hidden) poll();
  }, 15000);
})();
