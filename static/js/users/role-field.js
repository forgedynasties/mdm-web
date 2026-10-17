// Loaded by templates/users.html.
      (function(){
        var role=document.getElementById('f-role'), row=document.getElementById('f-restaurants-row');
        if(!role||!row) return;
        function sync(){ row.hidden = role.value !== 'owner'; }
        role.addEventListener('change', sync); sync();
      })();
      
