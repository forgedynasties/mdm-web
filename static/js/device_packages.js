// Loaded by templates/device_packages.html.
function filterPackages(q) {
  q = q.toLowerCase();
  const rows = document.querySelectorAll('#pkg-table tbody tr:not(#empty-row)');
  let visible = 0;
  rows.forEach(row => {
    const appName = row.cells[0] ? row.cells[0].textContent.toLowerCase() : '';
    const name = row.cells[1] ? row.cells[1].textContent.toLowerCase() : '';
    const show = appName.includes(q) || name.includes(q);
    row.style.display = show ? '' : 'none';
    if (show) visible++;
  });
  document.getElementById('no-match').style.display = (rows.length > 0 && visible === 0) ? '' : 'none';
}
