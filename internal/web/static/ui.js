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
