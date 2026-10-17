// Loaded by templates/layout.html.
      // Every labelled back-link (← Fleet, ← AT0031, ← Groups, …) should return to the
      // ACTUAL previous page the user came from, not its hard-coded default. When there's
      // an in-app page to go back to — the user navigated here from a same-origin page, or
      // has moved around inside the app (a boosted nav pushed history) — Back walks history
      // instead of following the label's href. On a direct load (no in-app history) it falls
      // back to the labelled href so Back still works.
      (function () {
        var hadInApp = !!(document.referrer && document.referrer.indexOf(location.origin + '/') === 0);
        document.addEventListener('htmx:pushedIntoHistory', function () { hadInApp = true; });
        document.addEventListener('click', function (e) {
          var a = e.target.closest && e.target.closest('.back-link');
          if (!a || e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey) return;
          if (hadInApp && history.length > 1) { e.preventDefault(); history.back(); }
        });
      })();
    
