// Public account pages: show/hide password, strength meter, match line. Advice only: the server enforces the length.
(function(){
  var d=document;
  d.querySelectorAll('.au1-eye').forEach(function(b){
    b.addEventListener('click',function(){
      var i=d.getElementById(b.getAttribute('data-for'));if(!i)return;
      var show=i.type==='password';i.type=show?'text':'password';
      b.textContent=show?'Hide':'Show';b.setAttribute('aria-label',show?'Hide password':'Show password');
    });
  });
  var set=d.querySelector('[data-au-set]');if(!set)return;
  var n=set.querySelector('[data-au-n]'),c=set.querySelector('[data-au-c]');
  var bars=set.querySelectorAll('.au1-pwm i'),lbl=set.querySelector('[data-au-lbl]'),mt=set.querySelector('[data-au-match]'),rules=set.querySelectorAll('[data-r]');
  function upd(){
    var v=n.value,w=c.value;
    var r={len:v.length>=8,case:/[a-z]/.test(v)&&/[A-Z]/.test(v),num:/\d/.test(v),long:v.length>=12};
    rules.forEach(function(li){li.classList.toggle('ok',!!r[li.getAttribute('data-r')])});
    var s=v.length===0?0:(r.len?1+(r.case?1:0)+(r.num?1:0)+(r.long?1:0):1);if(s>4)s=4;
    var cls=['','r','o','y','g'][s];
    bars.forEach(function(b,i){b.className=i<s?cls:''});
    lbl.textContent=v.length===0?'Enter a password':(!r.len?'Too short (needs 8)':['','','Fair','Good','Strong'][s]);
    mt.textContent=w.length===0?'':(w===v?'Passwords match':'Don’t match yet');
  }
  n.addEventListener('input',upd);c.addEventListener('input',upd);
})();
