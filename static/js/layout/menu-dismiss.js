// Every menu closes on a click outside it and on Escape (user, 23 Oct 2026: "do that for
// all the menus everywhere"). One handler for the whole dashboard, loaded by layout.html,
// so a page's own menu code only has to open and close its menu.
//
// What counts as a menu here:
//  - a <details> that is open and whose content floats (position absolute or fixed): the
//    ⋯ menus, rename and nickname popovers. Accordions (settings rows, folds) have content
//    in the flow and are left alone.
//  - a visible .cc-menu / [role=menu] that floats, opened by script (hidden attribute).
// A click on a menu's own trigger (aria-haspopup / aria-expanded / data-vt-trig, or the
// details' summary) is left to that trigger, so toggling keeps working.
(function () {
  if (window.__menuDismiss) return;
  window.__menuDismiss = true;

  function floats(el) {
    if (!el || el.nodeType !== 1) return false;
    var p = getComputedStyle(el).position;
    return p === 'absolute' || p === 'fixed';
  }
  function popoverDetails() {
    return Array.prototype.filter.call(document.querySelectorAll('details[open]'), function (d) {
      for (var c = d.firstElementChild; c; c = c.nextElementSibling) {
        if (c.tagName !== 'SUMMARY' && floats(c)) return true;
      }
      return false;
    });
  }
  function scriptMenus() {
    return Array.prototype.filter.call(document.querySelectorAll('.cc-menu:not([hidden]), [role="menu"]:not([hidden])'), function (m) {
      return !m.closest('details') && m.offsetParent !== null && floats(m);
    });
  }
  function triggersFor(m) {
    var out = [];
    if (m.id) document.querySelectorAll('[aria-controls="' + m.id + '"]').forEach(function (t) { out.push(t); });
    return out;
  }
  function closeMenu(m) {
    m.setAttribute('hidden', '');
    triggersFor(m).forEach(function (t) { t.setAttribute('aria-expanded', 'false'); });
  }
  function isTrigger(t) {
    return !!(t && t.closest && t.closest('[aria-haspopup], [aria-expanded], [data-vt-trig]'));
  }

  // Only menus already open when the click began are candidates: a menu its own button
  // opens during this click is never closed by the same click, whatever that button is.
  var wasOpen = [];
  document.addEventListener('click', function () {
    wasOpen = popoverDetails().concat(scriptMenus());
  }, true);
  document.addEventListener('click', function (e) {
    var t = e.target, before = wasOpen;
    wasOpen = [];
    before.forEach(function (m) {
      if (m.contains(t)) return;
      if (m.tagName === 'DETAILS') { if (m.open) m.open = false; return; }
      if (isTrigger(t) || m.hasAttribute('hidden')) return;
      closeMenu(m);
    });
  });

  document.addEventListener('keydown', function (e) {
    if (e.key !== 'Escape') return;
    var pd = popoverDetails(), sm = scriptMenus();
    if (!pd.length && !sm.length) return;
    // Focus goes back to what opened the menu, so the keyboard is not lost.
    var back = null;
    pd.forEach(function (d) {
      if (d.contains(document.activeElement)) back = d.querySelector('summary');
      d.open = false;
    });
    sm.forEach(function (m) {
      if (m.contains(document.activeElement)) back = triggersFor(m)[0] || back;
      closeMenu(m);
    });
    if (back && back.focus) back.focus();
  });
})();
