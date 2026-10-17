// Selección de registros para eliminar en la grilla de datos (la confirmación es de confirm.js).
(function () {
  var form = document.getElementById('del-sel');
  var grid = document.getElementById('grid');
  if (!form || !grid) return;
  var all = grid.querySelector('[data-sel-all]');
  var boxes = function () { return grid.querySelectorAll('input[name="k"][form="del-sel"]'); };
  var update = function () {
    var list = boxes(), n = 0;
    for (var i = 0; i < list.length; i++) {
      if (list[i].checked) n++;
      list[i].closest('tr').classList.toggle('selected', list[i].checked);
    }
    form.hidden = n === 0;
    form.querySelector('[data-sel-count]').textContent = n === 1 ? '1 registro seleccionado' : n + ' registros seleccionados';
    form.setAttribute('data-confirm', n === 1 ? '¿Eliminar 1 registro de ' + form.dataset.table + '?'
      : '¿Eliminar ' + n + ' registros de ' + form.dataset.table + '?');
    if (all) {
      all.checked = n > 0 && n === list.length;
      all.indeterminate = n > 0 && n < list.length;
    }
  };
  grid.addEventListener('change', function (e) {
    if (e.target === all) {
      var list = boxes();
      for (var i = 0; i < list.length; i++) list[i].checked = all.checked;
    }
    update();
  });
  form.querySelector('[data-sel-clear]').addEventListener('click', function () {
    var list = boxes();
    for (var i = 0; i < list.length; i++) list[i].checked = false;
    update();
  });
  update();
})();
