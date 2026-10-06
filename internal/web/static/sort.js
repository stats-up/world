// Ordena las tablas .sortable al hacer clic en un encabezado con data-sort ("num" o "text").
// El valor de cada celda sale de data-v (o de su texto).
document.addEventListener("click", (ev) => {
  const th = ev.target.closest("table.sortable th[data-sort]");
  if (!th) return;
  const table = th.closest("table");
  const col = Array.from(th.parentNode.children).indexOf(th);
  const num = th.dataset.sort === "num";
  const asc = th.getAttribute("aria-sort") === "descending";
  table.querySelectorAll("th[aria-sort]").forEach((h) => h.removeAttribute("aria-sort"));
  th.setAttribute("aria-sort", asc ? "ascending" : "descending");
  const val = (tr) => {
    const td = tr.children[col];
    const v = td ? (td.dataset.v ?? td.textContent.trim()) : "";
    return num ? parseFloat(v) || 0 : v.toLowerCase();
  };
  const body = table.tBodies[0];
  const rows = Array.from(body.rows);
  rows.sort((a, b) => {
    const x = val(a), y = val(b);
    const c = num ? x - y : x.localeCompare(y, "es");
    return asc ? c : -c;
  });
  rows.forEach((r) => body.appendChild(r));
});
