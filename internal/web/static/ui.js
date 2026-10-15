// Interacciones pequeñas del panel (sin dependencias).

// Filtros de tablas (se combinan todos los de una misma tabla):
//   <input data-filter="#tabla">                         busca en el texto de cada fila
//   <select data-filter="#tabla" data-filter-key="mode">  compara con data-mode de la fila ("" = todos)
// Opcionales: #tabla-empty (aviso sin resultados) y #tabla-count (filas visibles).
function applyFilters(sel) {
  const table = document.querySelector(sel);
  if (!table) return;
  const controls = document.querySelectorAll(`[data-filter="${sel}"]`);
  let visible = 0;
  table.querySelectorAll("tbody tr").forEach((tr) => {
    let show = true;
    controls.forEach((c) => {
      const v = c.value.trim().toLowerCase();
      if (!v) return;
      const key = c.dataset.filterKey;
      if (key) show = show && (tr.dataset[key] || "") === v;
      else show = show && tr.textContent.toLowerCase().includes(v);
    });
    tr.hidden = !show;
    if (show) visible++;
  });
  const empty = document.querySelector(sel + "-empty");
  if (empty) empty.hidden = visible > 0;
  const count = document.querySelector(sel + "-count");
  if (count) count.textContent = visible;
}
for (const type of ["input", "change"]) {
  document.addEventListener(type, (ev) => {
    const c = ev.target.closest("[data-filter]");
    if (c) applyFilters(c.dataset.filter);
  });
}

// Filas clickeables: <tr data-href="/ruta"> (los enlaces y botones de la fila siguen funcionando).
document.addEventListener("click", (ev) => {
  const tr = ev.target.closest("tr[data-href]");
  if (!tr || ev.target.closest("a, button, input, select, textarea, label")) return;
  if (ev.ctrlKey || ev.metaKey) window.open(tr.dataset.href, "_blank");
  else window.location.href = tr.dataset.href;
});

// Logs en vivo: <pre data-follow id="..."> queda en la última línea cada vez que htmx lo
// actualiza (deploys, importaciones de correo). Si el usuario subió para leer, se respeta
// su posición; al volver abajo, sigue de nuevo.
const atBottom = (el) => el.scrollHeight - el.scrollTop - el.clientHeight < 24;
const followState = new Map();
function followLogs() {
  document.querySelectorAll("pre[data-follow]").forEach((pre) => {
    const prev = followState.get(pre.id);
    pre.scrollTop = prev && !prev.bottom ? prev.top : pre.scrollHeight;
  });
  followState.clear();
}
document.addEventListener("htmx:beforeSwap", (ev) => {
  ev.detail.target.querySelectorAll("pre[data-follow]").forEach((pre) => {
    if (pre.id) followState.set(pre.id, { bottom: atBottom(pre), top: pre.scrollTop });
  });
});
document.addEventListener("htmx:afterSettle", followLogs);
document.addEventListener("DOMContentLoaded", followLogs);
// Un <details> recién abierto: su log no tenía tamaño mientras estaba cerrado.
document.addEventListener("toggle", (ev) => {
  const pre = ev.target.open && ev.target.querySelector?.("pre[data-follow]");
  if (pre) pre.scrollTop = pre.scrollHeight;
}, true);
