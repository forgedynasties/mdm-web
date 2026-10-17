// Loaded by templates/group_form.html.
function gfSync(){
  var name = document.getElementById('group-name').value.trim();
  var chip = document.getElementById('gf-chip-name');
  chip.textContent = name || 'Group name';
  chip.classList.toggle('muted', name === '');
  document.getElementById('gf-sum-name').textContent = name || 'a new group';
  document.getElementById('create-group-btn').disabled = name === '';
}
gfSync();
