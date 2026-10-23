// Loaded by templates/owner_home.html.
  var OH_HELP = { support: 'Request support', problem: 'Report a problem', replace: 'Ask for a replacement', question: 'Ask a question' };
  function ohHelp(kind) { document.getElementById('oh-help-kind').value = kind; document.getElementById('oh-help-t').textContent = OH_HELP[kind] || 'Help'; document.getElementById('oh-help').classList.add('modal-open'); setTimeout(function () { document.getElementById('oh-help-msg').focus(); }, 50); }
  function ohHelpSend(e) {
    e.preventDefault();
    // Not connected to anything yet (owner home preview): never claim it was sent.
    return false;
  }
