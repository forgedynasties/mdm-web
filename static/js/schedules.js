// Loaded by templates/schedules.html.
// Friendly schedule builder: users choose Daily/Weekly/Monthly/etc. and enter
// times in their LOCAL timezone; we compose the 5-field cron the server expects
// (which runs in UTC), converting the local time — and shifting the weekday /
// day-of-month when that conversion crosses midnight.
var SCH_DOW = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];

function schPad(n) { return (n < 10 ? '0' : '') + n; }

function schCurFreq() {
  var b = document.querySelector('#sch-freq .sfb.on');
  return b ? b.dataset.f : 'daily';
}

function schPick(f) {
  document.querySelectorAll('#sch-freq .sfb').forEach(function (b) { var sel = b.dataset.f === f; b.classList.toggle('on', sel); b.setAttribute('aria-checked', sel ? 'true' : 'false'); });
  document.querySelectorAll('.sch-panel').forEach(function (p) { p.hidden = p.dataset.p !== f; });
  schUpdate();
}

// toUTC converts a local "HH:MM" to a UTC hour/minute plus a day shift
// (-1, 0, or +1) for when the conversion crosses midnight.
function schToUTC(hhmm) {
  var pp = (hhmm || '00:00').split(':');
  var mins = (parseInt(pp[0], 10) || 0) * 60 + (parseInt(pp[1], 10) || 0);
  var t = mins + new Date().getTimezoneOffset(); // local + offset = UTC
  var shift = Math.floor(t / 1440);
  t = ((t % 1440) + 1440) % 1440;
  return { h: Math.floor(t / 60), m: t % 60, shift: shift };
}

function schFmt12(hhmm) {
  var pp = (hhmm || '00:00').split(':');
  var h = parseInt(pp[0], 10) || 0, m = parseInt(pp[1], 10) || 0;
  var ap = h < 12 ? 'AM' : 'PM', h12 = h % 12; if (h12 === 0) h12 = 12;
  return h12 + ':' + schPad(m) + ' ' + ap;
}

function schBuild() {
  var f = schCurFreq();
  if (f === 'daily') {
    var t = document.getElementById('sch-time-d').value || '03:00', u = schToUTC(t);
    return { cron: u.m + ' ' + u.h + ' * * *', desc: 'Every day at ' + schFmt12(t), local: true };
  }
  if (f === 'weekly') {
    var days = Array.prototype.map.call(document.querySelectorAll('#sch-days input:checked'), function (x) { return parseInt(x.value, 10); });
    if (!days.length) return { desc: 'Pick at least one day.', err: true };
    var t = document.getElementById('sch-time-w').value || '03:00', u = schToUTC(t);
    var ud = days.map(function (d) { return ((d + u.shift) % 7 + 7) % 7; });
    ud = ud.filter(function (v, i) { return ud.indexOf(v) === i; }).sort(function (a, b) { return a - b; });
    var names = days.slice().sort(function (a, b) { return a - b; }).map(function (d) { return SCH_DOW[d]; });
    return { cron: u.m + ' ' + u.h + ' * * ' + ud.join(','), desc: 'Every ' + names.join(', ') + ' at ' + schFmt12(t), local: true };
  }
  if (f === 'monthly') {
    var dom = parseInt(document.getElementById('sch-dom').value, 10) || 1;
    var t = document.getElementById('sch-time-m').value || '03:00', u = schToUTC(t);
    var ud = dom + u.shift; if (ud < 1) ud += 31; if (ud > 31) ud -= 31;
    return { cron: u.m + ' ' + u.h + ' ' + ud + ' * *', desc: 'On day ' + dom + ' of each month at ' + schFmt12(t), local: true };
  }
  if (f === 'minutes') {
    var n = parseInt(document.getElementById('sch-min').value, 10) || 15;
    if (n >= 60) return { cron: '0 * * * *', desc: 'Every hour' };
    return { cron: '*/' + n + ' * * * *', desc: 'Every ' + n + ' minutes' };
  }
  // advanced
  var cron = (document.getElementById('sch-cron-raw').value || '').trim();
  return { cron: cron, desc: cron ? 'Custom: ' + cron : 'Enter a cron expression.', err: !cron };
}

function schUpdate() {
  var r = schBuild();
  document.getElementById('sch-cron').value = r.cron || '';
  var s = document.getElementById('sch-sum');
  if (r.err || !r.cron) {
    s.className = 'sch-sum warn';
    s.textContent = r.desc;
    return;
  }
  var tz = r.local ? ' <span class="sch-tz">(your local time)</span>' : '';
  s.className = 'sch-sum';
  s.innerHTML = 'Runs <b>' + r.desc + '</b>' + tz + '<span class="sch-cronx">' + r.cron + ' UTC</span>';
}

function schSync() {
  schUpdate();
  if (!document.getElementById('sch-cron').value) {
    var s = document.getElementById('sch-sum');
    if (s) { s.className = 'sch-sum warn'; if (!s.textContent) s.textContent = 'Please complete the schedule above.'; }
    return false;
  }
  return true;
}

(function () {
  if (!document.getElementById('sch-cron')) return; // form not shown (viewer)
  var dom = document.getElementById('sch-dom');
  if (dom) for (var i = 1; i <= 31; i++) { var o = document.createElement('option'); o.value = i; o.textContent = i; dom.appendChild(o); }
  schUpdate();
})();
