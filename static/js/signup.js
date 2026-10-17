// Loaded by templates/signup.html.
function authTogglePw(fieldId,onId,offId,btnId){
  var i=document.getElementById(fieldId),on=document.getElementById(onId),off=document.getElementById(offId),b=document.getElementById(btnId);
  var show=i.type==='password'; i.type=show?'text':'password';
  on.style.display=show?'none':''; off.style.display=show?'':'none';
  b.setAttribute('aria-label',show?'Hide password':'Show password');
}
