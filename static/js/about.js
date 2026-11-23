// Public /about page: theme toggle, the live-feeling mock Overview, dock nav.
// Mock data only (fictional venues and serials); nothing is fetched from the server.
(function(){
  var root=document.documentElement;
  document.getElementById('theme').addEventListener('click',function(){
    var n=root.getAttribute('data-theme')==='dark'?'light':'dark';
    root.setAttribute('data-theme',n);
    try{localStorage.setItem('mdm-theme',n)}catch(e){}
  });

  var reduce=window.matchMedia&&matchMedia('(prefers-reduced-motion: reduce)').matches;

  /* radar */
  var axes=['Online','Checked in','Battery','No alerts','Compliance','Locked','Clients current'];
  var vals=[.9,.94,.78,.88,.82,.86,.9];
  function radar(svg,v,labels){
    var cx=100,cy=95,R=70,n=7,s='';
    function pt(i,r){var a=-Math.PI/2+i*2*Math.PI/n;return [cx+r*Math.cos(a),cy+r*Math.sin(a)]}
    for(var k=1;k<=3;k++){var p=[];for(var i=0;i<n;i++)p.push(pt(i,R*k/3).join(','));s+='<polygon class="grid" points="'+p.join(' ')+'"/>'}
    for(i=0;i<n;i++){var e=pt(i,R);s+='<line class="grid" x1="'+cx+'" y1="'+cy+'" x2="'+e[0]+'" y2="'+e[1]+'"/>';if(labels){var l=pt(i,R+12);s+='<text x="'+l[0]+'" y="'+(l[1]+3)+'" text-anchor="middle">'+(i+1)+'</text>'}}
    var q=[];for(i=0;i<n;i++)q.push(pt(i,R*v[i]).join(','));
    s+='<polygon class="shape" points="'+q.join(' ')+'"/>';
    svg.innerHTML=s;
  }
  var r1=document.getElementById('radar'),r2=document.getElementById('radar2');
  function drawRadars(){radar(r1,vals,true);radar(r2,vals,false);
    document.getElementById('hlist').innerHTML=axes.slice(0,4).map(function(a,i){var p=Math.round(vals[i]*100);return '<li><span>'+(i+1)+' · '+a+'</span><span class="pill '+(p>=80?'g':'r')+'">'+p+'%</span></li>'}).join('');
    document.getElementById('score').textContent=Math.round(vals.reduce(function(a,b){return a+b},0)/vals.length*100);
  }
  drawRadars();

  /* feed + detail (fictional data) */
  var items=[
    {k:'all',lvl:'warn',t:'Harbour Noodle Bar: 2 offline',s:'Watch · 4 of 6 online',badge:'WARNING',
     rows:[['Menu board','2 / 2',100],['Kiosk','1 / 2',50],['POS','1 / 2',50]],
     off:[['KS22-0418','Kiosk','14 min ago'],['PT-88102','POS','9 min ago']]},
    {k:'all',lvl:'bad',t:'18 crashes in the last 24 h',s:'Up from 6 the day before',badge:'ALERT',
     rows:[['Ordering app','12',80],['Menu player','4',30],['Payments','2',15]],
     off:[['T7-20931','TSAI','3 crashes'],['T7-20877','TSAI','2 crashes']]},
    {k:'all',lvl:'warn',t:'Low battery',s:'T7-21044 · Oak & Ember · 12%',badge:'WARNING',
     rows:[['Battery','12%',12],['Charger','Not docked',0],['Temp','33°C',40]],
     off:[['T7-21044','TSAI','Table 14']]},
    {k:'all',lvl:'warn',t:'Running hot',s:'KDS-7C21 · Little Saffron · 71°C',badge:'WARNING',
     rows:[['CPU temp','71°C',88],['Uptime','9 d',60],['Wi-Fi','-71 dBm',35]],
     off:[['KDS-7C21','KDS','Grill station']]},
    {k:'roll',lvl:'ok',t:'Ordering 4.12 rolling out',s:'Wave 2 of 3 · 64% installed',badge:'ROLLOUT',
     rows:[['Pilot venues','12 / 12',100],['Wave 2','203 / 318',64],['Everyone','0 / 1,140',0]],
     off:[['KS18-0211','Kiosk','Queued'],['T7-20512','TSAI','Downloading']]},
    {k:'roll',lvl:'ok',t:'Firmware 2.1.31 scheduled',s:'Tonight 02:00 · 86 kiosks',badge:'SCHEDULED',
     rows:[['Kiosk 18','30',100],['Kiosk 22','41',100],['Kiosk 27','15',100]],
     off:[['Window','02:00 to 05:00','Local time']]}
  ];
  var feed=document.getElementById('feed'),det=document.getElementById('detail'),cur=0,filter='all';
  function renderFeed(){
    feed.innerHTML='';
    items.forEach(function(it,i){
      if(filter!=='all'&&it.k!==filter)return;
      var li=document.createElement('li'),b=document.createElement('button');
      b.type='button';b.className=it.lvl;b.setAttribute('aria-pressed',i===cur?'true':'false');
      b.innerHTML='<b></b><span></span>';b.querySelector('b').textContent=it.t;b.querySelector('span').textContent=it.s;
      b.addEventListener('click',function(){cur=i;renderFeed();renderDetail()});
      li.appendChild(b);feed.appendChild(li);
    });
  }
  function esc(s){return String(s).replace(/[&<>"]/g,function(c){return{'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]})}
  function renderDetail(){
    var it=items[cur],bc=it.lvl==='bad'?'bad':it.lvl==='ok'?'ok':'';
    var h='<span class="badge '+bc+'">'+it.badge+'</span><h4>'+esc(it.t)+'</h4><div class="sub">'+esc(it.s)+'</div>';
    h+='<table class="dt"><tr><th>ITEM</th><th>VALUE</th><th style="width:40%"></th></tr>';
    it.rows.forEach(function(r){h+='<tr><td>'+esc(r[0])+'</td><td>'+esc(r[1])+'</td><td><div class="bar"><i style="width:'+r[2]+'%"></i></div></td></tr>'});
    h+='</table><table class="dt" style="margin-top:8px"><tr><th>DEVICE</th><th>TYPE</th><th>STATUS</th></tr>';
    it.off.forEach(function(r){h+='<tr><td class="mono">'+esc(r[0])+'</td><td>'+esc(r[1])+'</td><td>'+esc(r[2])+'</td></tr>'});
    h+='</table><span class="open">Open details ↗</span>';
    det.innerHTML=h;
  }
  document.querySelectorAll('.tab').forEach(function(t){t.addEventListener('click',function(){
    document.querySelectorAll('.tab').forEach(function(x){x.classList.remove('on')});t.classList.add('on');
    filter=t.dataset.f;cur=items.findIndex(function(it){return filter==='all'||it.k===filter});renderFeed();renderDetail();
  })});
  renderFeed();renderDetail();

  /* rings */
  var types=[['TSAI',812,880,'var(--ok)'],['Kiosk',301,318,'var(--ok)'],['KDS',176,190,'var(--ok)'],['POS',140,164,'var(--warn)']];
  var ringsEl=document.getElementById('rings');
  function drawRings(){
    var C=2*Math.PI*20;
    ringsEl.innerHTML=types.map(function(t){var f=t[1]/t[2];
      return '<div class="ring"><svg viewBox="0 0 48 48"><circle class="tr" cx="24" cy="24" r="20"/><circle class="vl" cx="24" cy="24" r="20" stroke="'+t[3]+'" stroke-dasharray="'+(C*f).toFixed(1)+' '+C.toFixed(1)+'"/></svg><div class="n">'+Math.round(f*100)+'%</div>'+t[0]+'</div>'}).join('');
  }
  drawRings();

  /* gentle live ticks */
  var on=1184,off=37,wait=22,w2=64;
  function fmt(n){return n.toLocaleString('en-GB')}
  var term=['logcat -d | tail -3','pm list packages | grep aio','getprop ro.build.version.release'],ti=0,ci=0,typed=document.getElementById('typed');
  if(!reduce){
    setInterval(function(){
      var d=Math.round((Math.random()-.45)*6);on=Math.max(1150,Math.min(1220,on+d));off=Math.max(28,Math.min(48,off-Math.sign(d)));
      document.getElementById('n-on').textContent=fmt(on);document.getElementById('n-off').textContent=off;
      document.getElementById('bar-a').style.flexGrow=on;document.getElementById('bar-b').style.flexGrow=off*5;
      vals=vals.map(function(v){return Math.max(.7,Math.min(.97,v+(Math.random()-.5)*.03))});drawRadars();
      types.forEach(function(t){t[1]=Math.max(Math.round(t[2]*.8),Math.min(t[2],t[1]+Math.round((Math.random()-.4)*3)))});drawRings();
      w2=Math.min(99,w2+1);document.getElementById('w2').style.width=w2+'%';document.getElementById('w2n').textContent=w2+'%';
      document.getElementById('t-bat').textContent=(78+Math.round(Math.random()*6))+'%';
      document.getElementById('t-tmp').textContent=(39+Math.round(Math.random()*4))+'°';
      document.getElementById('t-wifi').textContent=-(55+Math.round(Math.random()*6));
    },2600);
    setInterval(function(){
      var s=term[ti];
      if(ci<=s.length){typed.textContent=s.slice(0,ci++)}
      else if(ci<s.length+20){ci++}
      else{ci=0;ti=(ti+1)%term.length}
    },90);
  } else { typed.textContent=term[0]; }

  /* dock highlighting */
  var links=[].slice.call(document.querySelectorAll('.dock a'));
  var secs=links.map(function(a){return document.querySelector(a.getAttribute('href'))});
  if('IntersectionObserver' in window){
    var io=new IntersectionObserver(function(es){es.forEach(function(e){if(e.isIntersecting){var i=secs.indexOf(e.target);links.forEach(function(l,j){l.classList.toggle('on',j===i)})}})},{rootMargin:'-45% 0px -50% 0px'});
    secs.forEach(function(s){if(s)io.observe(s)});
  }
})();
