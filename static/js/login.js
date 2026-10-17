// Loaded by templates/login.html.
function authTogglePw(){
  var i=document.getElementById('password'),on=document.getElementById('pw-eye-on'),off=document.getElementById('pw-eye-off'),b=document.getElementById('pw-toggle');
  var show=i.type==='password'; i.type=show?'text':'password';
  on.style.display=show?'none':''; off.style.display=show?'':'none';
  b.setAttribute('aria-label',show?'Hide password':'Show password');
}
