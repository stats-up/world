// Confirmación de acciones delicadas (eliminar, vaciar, restaurar) con SweetAlert2.
// Uso: <form data-confirm="¿Eliminar el buzón?" data-confirm-text="Se borran todos sus correos."
//            data-confirm-button="Sí, eliminar"> ... </form>
// Si SweetAlert2 no cargó, se usa confirm() del navegador como respaldo.
(function () {
  document.addEventListener('submit', function (e) {
    var form = e.target;
    if (!(form instanceof HTMLFormElement) || !form.hasAttribute('data-confirm')) return;
    if (form.dataset.confirmed === '1') {
      delete form.dataset.confirmed; // se vuelve a pedir si el envío falla y se reintenta
      return;
    }
    e.preventDefault();
    var title = form.getAttribute('data-confirm');
    var text = form.getAttribute('data-confirm-text') || '';
    var button = form.getAttribute('data-confirm-button') || 'Sí, continuar';
    var submitter = e.submitter;
    var go = function () {
      form.dataset.confirmed = '1';
      if (form.requestSubmit) form.requestSubmit(submitter); else form.submit();
    };
    if (!window.Swal) {
      if (window.confirm(text ? title + '\n\n' + text : title)) go();
      return;
    }
    window.Swal.fire({
      icon: 'warning',
      title: title,
      text: text,
      showCancelButton: true,
      confirmButtonText: button,
      cancelButtonText: 'Cancelar',
      confirmButtonColor: '#b42318',
      reverseButtons: true,
      focusCancel: true
    }).then(function (r) { if (r.isConfirmed) go(); });
  }, true);
})();
