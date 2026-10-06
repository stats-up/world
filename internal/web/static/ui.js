// Interacciones pequeñas del panel (sin dependencias).

// Filtro de tablas: <input data-filter="#id-de-la-tabla"> oculta las filas que no contienen el texto.
document.addEventListener("input", (ev) => {
  const input = ev.target.closest("input[data-filter]");
  if (!input) return;
  const table = document.querySelector(input.dataset.filter);
  if (!table) return;
  const q = input.value.trim().toLowerCase();
  let visible = 0;
  table.querySelectorAll("tbody tr").forEach((tr) => {
    const show = !q || tr.textContent.toLowerCase().includes(q);
    tr.hidden = !show;
    if (show) visible++;
  });
  const empty = document.querySelector(input.dataset.filter + "-empty");
  if (empty) empty.hidden = visible > 0;
});

// Filas clickeables: <tr data-href="/ruta"> (los enlaces y botones de la fila siguen funcionando).
document.addEventListener("click", (ev) => {
  const tr = ev.target.closest("tr[data-href]");
  if (!tr || ev.target.closest("a, button, input, select, textarea, label")) return;
  if (ev.ctrlKey || ev.metaKey) window.open(tr.dataset.href, "_blank");
  else window.location.href = tr.dataset.href;
});
