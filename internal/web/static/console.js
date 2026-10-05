// Consola web: xterm.js conectado por WebSocket a una shell (docker exec con TTY) del contenedor del sitio.
// El teclado viaja como mensajes binarios; los cambios de tamaño, como JSON {"type":"resize"}.
(function () {
  const el = document.getElementById('terminal');
  const status = document.getElementById('console-status');
  const reconnect = document.getElementById('console-reconnect');
  if (!el || !window.Terminal) return;

  const dark = window.matchMedia('(prefers-color-scheme: dark)').matches;
  const term = new window.Terminal({
    cursorBlink: true,
    fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, monospace',
    fontSize: 14,
    scrollback: 5000,
    theme: { background: '#11161f', foreground: '#d6dde8', cursor: dark ? '#d6dde8' : '#ffffff' },
  });
  const fit = new window.FitAddon.FitAddon();
  term.loadAddon(fit);
  term.open(el);
  fit.fit();

  const encoder = new TextEncoder();
  let ws = null;

  function setStatus(text, kind) {
    status.textContent = text;
    status.className = 'badge ' + kind;
  }

  function sendSize() {
    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify({ type: 'resize', cols: term.cols, rows: term.rows }));
    }
  }

  function connect() {
    const proto = location.protocol === 'https:' ? 'wss://' : 'ws://';
    ws = new WebSocket(proto + location.host + el.dataset.ws);
    ws.binaryType = 'arraybuffer';
    reconnect.hidden = true;
    setStatus('conectando…', 'muted');

    ws.onopen = function () {
      setStatus('conectado', 'ok');
      sendSize();
      term.focus();
    };
    ws.onmessage = function (ev) {
      term.write(typeof ev.data === 'string' ? ev.data : new Uint8Array(ev.data));
    };
    ws.onclose = function (ev) {
      setStatus('desconectado', 'err');
      term.write('\r\n\x1b[33m[' + (ev.reason || 'Conexión cerrada') + ']\x1b[0m\r\n');
      reconnect.hidden = false;
    };
  }

  term.onData(function (d) {
    if (ws && ws.readyState === WebSocket.OPEN) ws.send(encoder.encode(d));
  });
  term.onResize(sendSize);
  window.addEventListener('resize', function () { fit.fit(); });
  reconnect.addEventListener('click', function () { term.reset(); connect(); });

  connect();
})();
